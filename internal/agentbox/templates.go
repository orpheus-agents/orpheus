package agentbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/templates"
)

type providerConfig struct {
	apiURL  string
	key     string
	timeout time.Duration
}

// Resolve once in Orpheus and pass the same endpoint to SDK and catalog HTTP calls.
func loadProviderConfig() (providerConfig, error) {
	cfg := providerConfig{key: os.Getenv("AGENTBOX_API_KEY"), timeout: 60 * time.Second}
	debug := false
	if value := os.Getenv("AGENTBOX_DEBUG"); value != "" {
		var err error
		debug, err = strconv.ParseBool(value)
		if err != nil {
			return cfg, errors.New("AGENTBOX_DEBUG must be a boolean")
		}
	}
	cfg.apiURL = os.Getenv("AGENTBOX_API_URL")
	if cfg.apiURL == "" {
		domain := os.Getenv("AGENTBOX_DOMAIN")
		if domain == "" {
			domain = "agentbox-runtime.ru"
		}
		cfg.apiURL = "https://api." + domain
		if debug {
			cfg.apiURL = "http://localhost:3000"
		}
	}
	parsed, err := url.Parse(cfg.apiURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return cfg, errors.New("invalid AgentBox API URL configuration")
	}
	cfg.apiURL = strings.TrimRight(cfg.apiURL, "/")
	return cfg, nil
}

type TemplateVerifier struct {
	config providerConfig
	client *http.Client
}

func NewTemplateVerifier() (*TemplateVerifier, error) {
	cfg, err := loadProviderConfig()
	if err != nil {
		return nil, err
	}
	return &TemplateVerifier{config: cfg, client: &http.Client{Timeout: cfg.timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func providerUnavailable() error {
	return session.Problem(503, "agentbox_unavailable", "AgentBox template verification is temporarily unavailable.")
}

func (v *TemplateVerifier) get(ctx context.Context, path string, out any) error {
	if v.config.key == "" {
		return providerUnavailable()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.config.apiURL+path, nil)
	if err != nil {
		return providerUnavailable()
	}
	req.Header.Set("X-API-KEY", v.config.key)
	res, err := v.client.Do(req)
	if err != nil {
		return providerUnavailable()
	}
	defer func() { _ = res.Body.Close() }()
	switch res.StatusCode {
	case http.StatusOK:
		raw, err := io.ReadAll(io.LimitReader(res.Body, 1024*1024+1))
		if err != nil || len(raw) > 1024*1024 || json.Unmarshal(raw, out) != nil {
			return providerUnavailable()
		}
		return nil
	case http.StatusBadRequest:
		return templates.Invalid("body", "template_name", "invalid_value", "AgentBox rejected the template reference.")
	case http.StatusForbidden:
		return templates.Invalid("body", "template_name", "template_forbidden", "This template is not owned by the configured AgentBox project.")
	case http.StatusNotFound:
		return templates.Invalid("body", "template_name", "template_not_found", "Template not found in AgentBox.")
	default:
		return providerUnavailable()
	}
}

func (v *TemplateVerifier) Verify(ctx context.Context, in templates.Input) (string, error) {
	normalized, err := templates.Normalize(in)
	if err != nil {
		return "", err
	}
	var alias struct {
		TemplateID string `json:"templateID"`
	}
	if err := v.get(ctx, "/templates/aliases/"+url.PathEscape(normalized.TemplateName), &alias); err != nil {
		return "", err
	}
	if strings.TrimSpace(alias.TemplateID) == "" {
		return "", providerUnavailable()
	}
	var tags []struct {
		Tag string `json:"tag"`
	}
	if err := v.get(ctx, "/templates/"+url.PathEscape(alias.TemplateID)+"/tags", &tags); err != nil {
		return "", err
	}
	if tags == nil {
		return "", providerUnavailable()
	}
	tag := "default"
	if normalized.Tag != nil {
		tag = *normalized.Tag
	}
	found := false
	for _, item := range tags {
		if item.Tag == "" {
			return "", providerUnavailable()
		}
		found = found || item.Tag == tag
	}
	if !found {
		message := "Tag not found in AgentBox."
		if normalized.Tag == nil {
			message = "The template has no default tag; choose an existing tag."
		}
		return "", templates.Invalid("body", "tag", "tag_not_found", message)
	}
	return alias.TemplateID, nil
}
