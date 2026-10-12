package servicecredentials

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/session"
)

func testConfig(endpoint string) Config {
	return Config{Endpoint: endpoint, SecretEnv: "TEST_PROVIDER_KEY", ContextFields: []string{"subject", "scope_context", "context_observed_at"}, TokenFile: "~/.kord/run.token"}
}
func testGrant() Grant {
	now := time.Now().UTC()
	return Grant{CredentialID: "grant", AccessToken: "stable-secret", ScopeKey: "scope", ExpiresAt: now.Add(290 * time.Second), RunExpiresAt: now.Add(1190 * time.Second)}
}
func TestIssueProjectionRenewIdentityAndRedaction(t *testing.T) {
	t.Setenv("TEST_PROVIDER_KEY", strings.Repeat("k", 32))
	grant := testGrant()
	var requests []map[string]json.RawMessage
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("k", 32) {
			t.Error("missing server authentication")
		}
		var input map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		requests = append(requests, input)
		_ = json.NewEncoder(w).Encode(grant)
	}))
	defer server.Close()
	c := Client{HTTP: server.Client()}
	cfg := testConfig(server.URL)
	access := session.AccessContext{InvocationID: uuid.New(), Subject: session.AccessSubject{Namespace: "mm", ID: "a"}, ScopeContext: session.AccessScopeContext{"audience_subjects": json.RawMessage(`["a","b"]`), "channel_id": json.RawMessage(`"c"`), "agent_id": json.RawMessage(`"bot"`), "policy_space_id": json.RawMessage(`"p"`)}, ContextObservedAt: time.Now(), AllowedInputSubjects: []session.AccessSubject{{Namespace: "mm", ID: "b"}}}
	got, err := c.Issue(t.Context(), cfg, "kord", access)
	if err != nil || got != grant {
		t.Fatal(got, err)
	}
	if len(requests[0]) != 7 {
		t.Fatal("unexpected issue projection", requests[0])
	}
	for field, value := range map[string]any{"operation": "issue", "service": "kord", "invocation_id": access.InvocationID, "subject": access.Subject, "scope_context": access.ScopeContext, "context_observed_at": access.ContextObservedAt} {
		want, err := json.Marshal(value)
		if err != nil || string(requests[0][field]) != string(want) {
			t.Fatalf("provider field %s changed: %s, want %s (%v)", field, requests[0][field], want, err)
		}
	}
	if _, exists := requests[0]["allowed_input_subjects"]; exists {
		t.Fatal("core input authority leaked to provider")
	}
	for range 2 {
		if _, err := c.Renew(t.Context(), cfg, "kord", access.InvocationID, got); err != nil {
			t.Fatal(err)
		}
	}
	if len(requests[1]) != 5 || string(requests[1]["request_id"]) != string(requests[2]["request_id"]) {
		t.Fatal("renew is not idempotent")
	}
	grant.ScopeKey = "changed"
	if _, err := c.Renew(t.Context(), cfg, "kord", access.InvocationID, got); err == nil {
		t.Fatal("accepted changed scope")
	}
	grant = got
	grant.AccessToken = "replacement"
	if _, err := c.Renew(t.Context(), cfg, "kord", access.InvocationID, got); err == nil {
		t.Fatal("accepted replacement bearer")
	}
	grant = got
	grant.RunExpiresAt = grant.RunExpiresAt.Add(time.Second)
	if _, err := c.Renew(t.Context(), cfg, "kord", access.InvocationID, got); err == nil {
		t.Fatal("accepted moved absolute deadline")
	}
}

func TestIssueHonorsEveryConfiguredProjectionSubset(t *testing.T) {
	t.Setenv("TEST_PROVIDER_KEY", strings.Repeat("k", 32))
	access := session.AccessContext{InvocationID: uuid.New(), Subject: session.AccessSubject{Namespace: "scheduler", ID: "cron"}, ScopeContext: session.AccessScopeContext{"job_id": json.RawMessage(`9007199254740993`), "nested": json.RawMessage(`{"value":1.2300e99}`)}, ContextObservedAt: time.Now(), AllowedInputSubjects: []session.AccessSubject{{Namespace: "users", ID: "operator"}}}
	var input map[string]json.RawMessage
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		input = nil
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(testGrant())
	}))
	defer server.Close()
	for mask := range 8 {
		cfg := testConfig(server.URL)
		cfg.ContextFields = nil
		for i, field := range projection {
			if mask&(1<<i) != 0 {
				cfg.ContextFields = append(cfg.ContextFields, field)
			}
		}
		if _, err := (Client{HTTP: server.Client()}).Issue(t.Context(), cfg, "source", access); err != nil {
			t.Fatal(mask, err)
		}
		if len(input) != 4+len(cfg.ContextFields) {
			t.Fatal("unexpected projection", mask, input)
		}
		for _, field := range projection {
			_, exists := input[field]
			if exists != slices.Contains(cfg.ContextFields, field) {
				t.Fatal("projection ignored", mask, field)
			}
		}
		if _, exists := input["allowed_input_subjects"]; exists {
			t.Fatal("core input authority leaked to provider")
		}
		if raw, exists := input["scope_context"]; exists {
			var scope session.AccessScopeContext
			if err := json.Unmarshal(raw, &scope); err != nil || !reflect.DeepEqual(scope, access.ScopeContext) {
				t.Fatal("opaque scope changed", string(raw), err)
			}
		}
	}
}
func TestProviderRejectsUnsafeResponses(t *testing.T) {
	t.Setenv("TEST_PROVIDER_KEY", strings.Repeat("k", 32))
	for _, test := range []struct {
		name   string
		status int
		body   func() string
	}{
		{"redirect", 302, func() string { return "secret response" }},
		{"denied", 403, func() string { return "secret response" }},
		{"conflict", 409, func() string { return "secret response" }},
		{"oversize", 200, func() string { return strings.Repeat("x", 65537) }},
		{"extra fields", 200, func() string { return `{"command":"run secret"}` }},
		{"trailing json", 200, func() string { b, _ := json.Marshal(testGrant()); return string(b) + ` {}` }},
		{"expired", 200, func() string {
			g := testGrant()
			g.ExpiresAt = time.Now().Add(-time.Second)
			b, _ := json.Marshal(g)
			return string(b)
		}},
		{"long lease", 200, func() string {
			g := testGrant()
			g.ExpiresAt = time.Now().Add(400 * time.Second)
			b, _ := json.Marshal(g)
			return string(b)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "https://example.invalid/")
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body()))
			}))
			defer server.Close()
			_, err := (Client{HTTP: server.Client()}).Issue(t.Context(), testConfig(server.URL), "kord", session.AccessContext{})
			if err == nil || strings.Contains(err.Error(), "secret response") {
				t.Fatal("unsafe provider response", err)
			}
		})
	}
}
func TestProviderConfigAndExpiry(t *testing.T) {
	good := testConfig("https://provider.invalid/credentials")
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"http", "userinfo", "query", "path", "projection", "duplicate projection", "authority projection", "metadata projection", "secret"} {
		c := good
		switch field {
		case "http":
			c.Endpoint = "http://localhost"
		case "userinfo":
			c.Endpoint = "https://user:pass@provider.invalid"
		case "query":
			c.Endpoint += "?key=x"
		case "path":
			c.TokenFile = "~/../../tmp/token"
		case "projection":
			c.ContextFields = []string{"messages"}
		case "duplicate projection":
			c.ContextFields = []string{"subject", "subject"}
		case "authority projection":
			c.ContextFields = []string{"allowed_input_subjects"}
		case "metadata projection":
			c.ContextFields = []string{"metadata"}
		case "secret":
			c.SecretEnv = "BAD\nKEY"
		}
		if c.Validate() == nil {
			t.Fatal("accepted", field)
		}
	}
	state := State{Entries: map[string]Entry{"kord": {Grant: testGrant()}}}
	if state.Expired(time.Now()) || !state.Expired(time.Now().Add(5*time.Minute)) {
		t.Fatal("lease boundary")
	}
}

func TestProviderClockSkewLimits(t *testing.T) {
	now := time.Date(2026, 10, 11, 15, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name            string
		lease, deadline time.Duration
		valid           bool
	}{
		{"aligned", 300 * time.Second, 1200 * time.Second, true},
		{"provider ahead", 303 * time.Second, 1203 * time.Second, true},
		{"provider behind", 297 * time.Second, 1197 * time.Second, true},
		{"exact tolerance", 305 * time.Second, 1205 * time.Second, true},
		{"lease beyond tolerance", 305*time.Second + time.Nanosecond, 1200 * time.Second, false},
		{"deadline beyond tolerance", 300 * time.Second, 1205*time.Second + time.Nanosecond, false},
		{"expired at boundary", 0, 900 * time.Second, false},
		{"recently expired", -time.Nanosecond, 900 * time.Second, false},
		{"lease beyond deadline", 2 * time.Second, time.Second, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			g := Grant{ExpiresAt: now.Add(test.lease), RunExpiresAt: now.Add(test.deadline)}
			if g.validLifetime(now) != test.valid {
				t.Fatalf("validLifetime = %v, want %v", g.validLifetime(now), test.valid)
			}
			state := State{Entries: map[string]Entry{"kord": {Grant: g}}}
			if !state.Expired(g.ExpiresAt) || !state.Expired(g.RunExpiresAt) {
				t.Fatal("clock tolerance extended the execution lifetime")
			}
		})
	}
}

func TestIssueAndRenewPreserveProviderTimestampsWithClockSkew(t *testing.T) {
	t.Setenv("TEST_PROVIDER_KEY", strings.Repeat("k", 32))
	providerNow := time.Now().UTC().Add(3 * time.Second)
	grant := testGrant()
	grant.ExpiresAt = providerNow.Add(300 * time.Second)
	grant.RunExpiresAt = providerNow.Add(1200 * time.Second)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(grant)
	}))
	defer server.Close()
	c := Client{HTTP: server.Client()}
	cfg := testConfig(server.URL)
	access := session.AccessContext{InvocationID: uuid.New()}
	issued, err := c.Issue(t.Context(), cfg, "kord", access)
	if err != nil || issued != grant {
		t.Fatal("issue changed or rejected provider timestamps", err)
	}
	renewed, err := c.Renew(t.Context(), cfg, "kord", access.InvocationID, issued)
	if err != nil || renewed != grant {
		t.Fatal("renew changed or rejected provider timestamps", err)
	}
}
