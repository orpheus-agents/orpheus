//go:build integration

package worker

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/servicecredentials"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

func workerCredentials(t *testing.T, s *store.Store, a session.Acceptance, grant servicecredentials.Grant, cfg servicecredentials.Config) servicecredentials.State {
	t.Helper()
	state := servicecredentials.State{Context: session.AccessContext{InvocationID: uuid.New(), Subject: session.AccessSubject{Namespace: "scheduler", ID: "nightly"}, ScopeContext: session.AccessScopeContext{"schedule": json.RawMessage(`"nightly"`)}, ContextObservedAt: time.Now()}, Entries: map[string]servicecredentials.Entry{"kord": {Grant: grant, Config: cfg}}}
	raw, _ := json.Marshal(state)
	token, err := s.Cipher.EncryptCredentials(a.SessionID, a.RunID, string(raw))
	if err != nil {
		t.Fatal(err)
	}
	access, _ := json.Marshal(state.Context)
	scopes, _ := json.Marshal(state.Scopes())
	err = db.New(s.Pool).InsertServiceCredentials(t.Context(), db.InsertServiceCredentialsParams{RunID: a.RunID, InvocationID: state.Context.InvocationID, AccessContext: access, Scopes: scopes, Ciphertext: *token})
	if err != nil {
		t.Fatal(err)
	}
	return state
}
func TestWorkerCredentialLifecycle(t *testing.T) {
	s, r, a, e := setup(t)
	t.Setenv("WORKER_PROVIDER_KEY", strings.Repeat("k", 32))
	var renewals atomic.Int32
	now := time.Now().UTC()
	grant := servicecredentials.Grant{CredentialID: "grant", AccessToken: "worker-private-token", ScopeKey: "scope", ExpiresAt: now.Add(20 * time.Second), RunExpiresAt: now.Add(1190 * time.Second)}
	renewed := grant
	renewed.ExpiresAt = now.Add(290 * time.Second)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(req.Body).Decode(&in)
		if in["operation"] != "renew" {
			t.Error("worker called issue")
		}
		renewals.Add(1)
		_ = json.NewEncoder(w).Encode(renewed)
	}))
	defer server.Close()
	cfg := servicecredentials.Config{Endpoint: server.URL, SecretEnv: "WORKER_PROVIDER_KEY", ContextFields: []string{"subject", "scope_context", "context_observed_at"}, TokenFile: "~/.kord/run.token"}
	s.CredentialClient = servicecredentials.Client{HTTP: server.Client()}
	workerCredentials(t, s, a, grant, cfg)
	writes, removes := 0, 0
	r.credentialWrite = func(path string, raw []byte) error {
		if path != "/home/template/.kord/.run.fixture" || string(raw) != grant.AccessToken {
			t.Error("wrong token staging")
		}
		if r.starts != 0 || r.launches != 0 {
			t.Error("token installed after model started")
		}
		writes++
		return nil
	}
	r.credentialCommands = func(command string) ([]byte, error) {
		if strings.Contains(command, grant.AccessToken) || strings.Contains(command, strings.Repeat("k", 32)) {
			t.Error("secret in shell command")
		}
		if strings.Contains(command, "mktemp") {
			if !strings.Contains(command, "umask 077") || !strings.Contains(command, "chmod 700") {
				t.Error("unsafe staging")
			}
			return []byte("/home/template/.kord/.run.fixture\n"), nil
		}
		if strings.Contains(command, `"$d/run.token"`) {
			removes++
		}
		return []byte("/home/template/workspace"), nil
	}
	tick(t, e)
	if writes != 1 || r.starts != 1 || renewals.Load() != 1 {
		t.Fatal("not prepared before launch", writes, r.starts, renewals.Load())
	}
	state, err := s.ServiceCredentials(t.Context(), a.SessionID, a.RunID)
	if err != nil || !state.Entries["kord"].Grant.ExpiresAt.Equal(renewed.ExpiresAt) {
		t.Fatal("renewal was not persisted", err)
	}
	// A restarted worker restores the same bearer without calling issue or renew.
	e.Disconnect()
	e = executor(a.SessionID, s, r)
	t.Cleanup(e.Disconnect)
	r.credentialWrite = func(_ string, raw []byte) error {
		if string(raw) != grant.AccessToken {
			t.Error("token changed on restore")
		}
		writes++
		return nil
	}
	tick(t, e)
	if writes != 2 || renewals.Load() != 1 || r.starts != 1 {
		t.Fatal("restart split run", writes, renewals.Load(), r.starts)
	}
	if _, err := s.Cancel(t.Context(), a.SessionID, a.RunID); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	if renewals.Load() != 1 || removes == 0 {
		t.Fatal("cancel did not remove credential/stop renew")
	}
}
func TestExpiredCredentialNeverLaunchesOrReissues(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(map[bool]string{false: "before preparation", true: "during execution"}[started], func(t *testing.T) {
			s, r, a, e := setup(t)
			if started {
				tick(t, e)
			}
			now := time.Now().UTC()
			cfg := servicecredentials.Config{Endpoint: "https://provider.invalid", SecretEnv: "WORKER_PROVIDER_KEY", ContextFields: []string{"subject", "scope_context", "context_observed_at"}, TokenFile: "~/.kord/run.token"}
			workerCredentials(t, s, a, servicecredentials.Grant{CredentialID: "grant", AccessToken: "private", ScopeKey: "scope", ExpiresAt: now.Add(-time.Second), RunExpiresAt: now.Add(10 * time.Minute)}, cfg)
			starts := r.starts
			tick(t, e)
			run, err := s.Run(t.Context(), a.SessionID, a.RunID)
			if err != nil || run.Status != session.Failed || run.Error.Code != "service_credentials_expired" || r.starts != starts {
				t.Fatal("expired grant executed", run, err)
			}
			if started && r.pauses == 0 {
				t.Fatal("live sandbox was not stopped")
			}
			tick(t, e)
		})
	}
}
func TestTokenWriteFailurePreventsModelStart(t *testing.T) {
	s, r, a, e := setup(t)
	now := time.Now().UTC()
	cfg := servicecredentials.Config{Endpoint: "https://provider.invalid", SecretEnv: "WORKER_PROVIDER_KEY", ContextFields: []string{"subject", "scope_context", "context_observed_at"}, TokenFile: "~/.kord/run.token"}
	workerCredentials(t, s, a, servicecredentials.Grant{CredentialID: "grant", AccessToken: "private", ScopeKey: "scope", ExpiresAt: now.Add(4 * time.Minute), RunExpiresAt: now.Add(15 * time.Minute)}, cfg)
	r.credentialCommands = func(command string) ([]byte, error) {
		if strings.Contains(command, "mktemp") {
			return []byte("/home/template/.kord/.run.fixture"), nil
		}
		return []byte("/home/template/workspace"), nil
	}
	r.credentialWrite = func(string, []byte) error { return errors.New("write failed") }
	if err := e.Tick(t.Context()); err == nil || r.starts != 0 || r.launches != 0 {
		t.Fatal("model launched without token", err, r.starts, r.launches)
	}
}
