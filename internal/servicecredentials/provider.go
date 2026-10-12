// Package servicecredentials implements the server-only issue/renew protocol.
// It never receives prompts, messages, model output, or source data.
package servicecredentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/session"
)

// Config is trusted server configuration, frozen in encrypted run state.
// The response cannot select a file path, environment variable or command.
type Config struct {
	Endpoint      string   `toml:"endpoint" json:"endpoint"`
	SecretEnv     string   `toml:"secret_env" json:"secret_env"`
	ContextFields []string `toml:"context_fields" json:"context_fields"`
	TokenFile     string   `toml:"token_file" json:"token_file"`
}

var envName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
var tokenPath = regexp.MustCompile(`^~/\.[a-z][a-z0-9_-]*/run\.token$`)
var projection = []string{"subject", "scope_context", "context_observed_at"}

func (c Config) Validate() error {
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !envName.MatchString(c.SecretEnv) || !tokenPath.MatchString(c.TokenFile) {
		return errors.New("invalid credentials provider configuration")
	}
	seen := map[string]bool{}
	for _, field := range c.ContextFields {
		if !slices.Contains(projection, field) || seen[field] {
			return errors.New("provider context projection must contain unique fields from subject, scope_context and context_observed_at")
		}
		seen[field] = true
	}
	return nil
}

type Grant struct {
	CredentialID string    `json:"credential_id"`
	AccessToken  string    `json:"access_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	RunExpiresAt time.Time `json:"run_expires_at"`
	ScopeKey     string    `json:"scope_key"`
}

// Providers and workers run on different hosts. This tolerance only bounds a
// future timestamp; it never extends an expired grant or changes its deadline.
const maxProviderClockSkew = 5 * time.Second

func (g Grant) validLifetime(now time.Time) bool {
	return now.Before(g.ExpiresAt) && !g.ExpiresAt.After(g.RunExpiresAt) &&
		!g.ExpiresAt.After(now.Add(300*time.Second+maxProviderClockSkew)) &&
		!g.RunExpiresAt.After(now.Add(1200*time.Second+maxProviderClockSkew))
}

type Entry struct {
	Config Config `json:"config"`
	Grant  Grant  `json:"grant"`
}
type State struct {
	Context session.AccessContext `json:"context"`
	Entries map[string]Entry      `json:"entries"`
}
type Client struct{ HTTP *http.Client }

func (c Client) call(ctx context.Context, cfg Config, body any) (Grant, error) {
	var out Grant
	fail := func() (Grant, error) {
		return Grant{}, session.Problem(503, "service_credentials_unavailable", "Service credentials are unavailable.")
	}
	if cfg.Validate() != nil {
		return fail()
	}
	secret := os.Getenv(cfg.SecretEnv)
	if len(secret) < 32 || strings.ContainsAny(secret, "\r\n") {
		return fail()
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fail()
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint, bytes.NewReader(raw))
	if err != nil {
		return fail()
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	// Never forward the server key through redirects or environment proxies.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, Timeout: 10 * time.Second}
	if c.HTTP != nil {
		client = *c.HTTP
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return fail()
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden, http.StatusUnauthorized:
		return Grant{}, session.Problem(403, "service_access_denied", "Provider denied access.")
	case http.StatusConflict:
		return Grant{}, session.Problem(409, "service_credentials_conflict", "Invocation context conflicts with the existing grant.")
	default:
		return fail()
	}
	raw, err = io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || len(raw) > 65536 {
		return fail()
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(&out); err != nil {
		return fail()
	}
	if err = d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return fail()
	}
	now := time.Now()
	if len(out.CredentialID) == 0 || len(out.CredentialID) > 256 || len(out.ScopeKey) == 0 || len(out.ScopeKey) > 1024 || len(out.AccessToken) == 0 || len(out.AccessToken) > 8192 || strings.ContainsAny(out.AccessToken, "\x00\r\n") || !out.validLifetime(now) {
		return fail()
	}
	return out, nil
}
func (c Client) Issue(ctx context.Context, cfg Config, service string, access session.AccessContext) (Grant, error) {
	// Keep the provider's projection separate from core-only run authority.
	// In particular, allowed_input_subjects never leaves Orpheus.
	body := map[string]any{"operation": "issue", "request_id": uuid.New(), "service": service, "invocation_id": access.InvocationID}
	for _, field := range cfg.ContextFields {
		switch field {
		case "subject":
			body[field] = access.Subject
		case "scope_context":
			body[field] = access.ScopeContext
		case "context_observed_at":
			body[field] = access.ContextObservedAt
		}
	}
	return c.call(ctx, cfg, body)
}
func (c Client) Renew(ctx context.Context, cfg Config, service string, invocation uuid.UUID, previous Grant) (Grant, error) {
	// Derived from the persisted lease: a lost response retries the same operation.
	requestID := uuid.NewSHA1(invocation, []byte(service+":"+previous.CredentialID+":"+previous.ExpiresAt.Format(time.RFC3339Nano)))
	next, err := c.call(ctx, cfg, struct {
		Operation    string    `json:"operation"`
		RequestID    uuid.UUID `json:"request_id"`
		Service      string    `json:"service"`
		InvocationID uuid.UUID `json:"invocation_id"`
		CredentialID string    `json:"credential_id"`
	}{"renew", requestID, service, invocation, previous.CredentialID})
	if err != nil {
		return Grant{}, err
	}
	if next.AccessToken != previous.AccessToken || next.CredentialID != previous.CredentialID || next.ScopeKey != previous.ScopeKey || !next.RunExpiresAt.Equal(previous.RunExpiresAt) || next.ExpiresAt.Before(previous.ExpiresAt) {
		return Grant{}, session.Problem(503, "service_credentials_unavailable", "Provider changed the frozen grant.")
	}
	return next, nil
}
func (s State) Scopes() map[string]string {
	out := map[string]string{}
	for code, e := range s.Entries {
		out[code] = e.Grant.ScopeKey
	}
	return out
}
func (s State) Expired(now time.Time) bool {
	for _, e := range s.Entries {
		if !now.Before(e.Grant.ExpiresAt) || !now.Before(e.Grant.RunExpiresAt) {
			return true
		}
	}
	return false
}
