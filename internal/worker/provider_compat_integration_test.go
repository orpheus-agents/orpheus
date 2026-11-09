//go:build integration

package worker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	public "github.com/orpheus-agents/orpheus/client"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/httpserver"
	"github.com/orpheus-agents/orpheus/internal/servicecredentials"
	"github.com/orpheus-agents/orpheus/internal/session"
)

// Optional cross-repository test: the fixture runs the real pinned Kord Provider
// dispatch and SQLite ledger. Only sandbox/model execution is simulated here.
func TestPinnedProviderNativeWireCompatibility(t *testing.T) {
	targetURL := os.Getenv("ORPHEUS_PROVIDER_TEST_URL")
	if targetURL == "" {
		t.Skip("cross-repository runner supplies synthetic Provider")
	}
	target, err := url.Parse(targetURL)
	if err != nil {
		t.Fatal(err)
	}
	bridge := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(target))
	defer bridge.Close()
	s, box, _, unused := setup(t)
	unused.Disconnect()
	t.Setenv("PROVIDER_COMPAT_KEY", "server-key-for-synthetic-native-test-01")
	cfg := servicecredentials.Config{Endpoint: bridge.URL + "/api/v1/agent-credentials", SecretEnv: "PROVIDER_COMPAT_KEY", ContextFields: []string{"scope_context", "context_observed_at"}, TokenFile: "~/.kord/run.token", MaxLeaseSeconds: 300, MaxRunSeconds: 1200}
	s.Profiles.Services = map[string]config.Service{"kord": {Name: "Kord", Description: "Synthetic pinned provider", Provider: &cfg}}
	s.CredentialClient = servicecredentials.Client{HTTP: bridge.Client()}
	s.Settings.PublicAPIKeys = []string{"synthetic-core-client"}
	handler, err := httpserver.Handler(s, t.Context())
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(handler)
	defer api.Close()
	access := session.AccessContext{InvocationID: uuid.New(), Subject: session.AccessSubject{Namespace: "scheduler", ID: "test-job"}, ScopeContext: session.AccessScopeContext{"actor_user_id": json.RawMessage(`5`), "reader_user_ids": json.RawMessage(`[1,2,3,4,5,6,7]`), "agent_id": json.RawMessage(`"cxassist"`), "policy_space_id": json.RawMessage(`"cx-test"`)}, ContextObservedAt: time.Now().UTC()}
	in := map[string]any{"allow_multiple_runs": true, "configuration": map[string]any{"agent": map[string]any{"profile": "p"}, "sandbox": map[string]any{"template": "codex", "services": []string{"kord"}}, "limits": map[string]any{"run_timeout_seconds": 600}}, "messages": []map[string]string{{"text": "synthetic work"}}, "access_context": access}
	raw, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(t.Context(), "POST", api.URL+"/api/v1/sessions", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer synthetic-core-client")
	req.Header.Set("Idempotency-Key", uuid.NewString())
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var accepted public.Accepted
	if response.StatusCode != 202 {
		var problem any
		_ = json.NewDecoder(response.Body).Decode(&problem)
		t.Fatalf("core admission status=%d problem=%v", response.StatusCode, problem)
	}
	if err = json.NewDecoder(response.Body).Decode(&accepted); err != nil {
		t.Fatal(err)
	}
	state, err := s.ServiceCredentials(t.Context(), accepted.SessionID, accepted.RunID)
	if err != nil {
		t.Fatal(err)
	}
	grant := state.Entries["kord"].Grant
	renewed, err := s.CredentialClient.Renew(t.Context(), cfg, "kord", access.InvocationID, grant)
	if err != nil || renewed.AccessToken != grant.AccessToken || renewed.ScopeKey != grant.ScopeKey || !renewed.RunExpiresAt.Equal(grant.RunExpiresAt) {
		t.Fatal("actual Provider renew incompatible", err)
	}
	authorize := func() int {
		b, _ := json.Marshal(map[string]string{"token": grant.AccessToken})
		r, err := http.Post(targetURL+"/authorize", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Body.Close() }()
		return r.StatusCode
	}
	if authorize() != 200 {
		t.Fatal("issued grant unavailable")
	}
	deadlineTokenFiles(box)
	installed := false
	box.credentialWrite = func(path string, raw []byte) error {
		if strings.HasSuffix(path, "/run.token") {
			installed = string(raw) == grant.AccessToken
		}
		return nil
	}
	e := executor(accepted.SessionID, s, box)
	t.Cleanup(e.Disconnect)
	tick(t, e)
	if box.starts != 1 || !installed {
		t.Fatal("actual bearer not installed before dispatch")
	}
	if _, err = s.Cancel(t.Context(), accepted.SessionID, accepted.RunID); err != nil {
		t.Fatal(err)
	}
	e.Disconnect()
	if err = s.RevokeTerminalCredentials(t.Context()); err != nil {
		t.Fatal(err)
	}
	if authorize() != 403 {
		t.Fatal("cancelled run retained Provider access")
	}
	if err = s.CredentialClient.Revoke(t.Context(), cfg, "kord", access.InvocationID, grant); err != nil {
		t.Fatal("revoke replay incompatible", err)
	}
	if _, err = s.CredentialClient.Renew(t.Context(), cfg, "kord", access.InvocationID, grant); err == nil {
		t.Fatal("revocation did not win over renewal")
	}
	// Current Kord does not accept generic source/subject fields: no silent fallback.
	cfg.ContextFields = append(cfg.ContextFields, "subject")
	access.InvocationID = uuid.New()
	if _, err = s.CredentialClient.Issue(t.Context(), cfg, "kord", access); err == nil {
		t.Fatal("unsupported projection was silently accepted")
	}
}
