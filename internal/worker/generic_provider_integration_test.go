//go:build integration

package worker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	public "github.com/orpheus-agents/orpheus/client"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/httpserver"
	"github.com/orpheus-agents/orpheus/internal/servicecredentials"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
)

const genericCoreKey = "synthetic-a9-core-key-00000000000000"
const genericProviderKey = "server-key-for-synthetic-native-test-01"
const genericProviderSourceKey = "synthetic-a9-provider-source-key-000000"
const genericSourceKey = "synthetic-a9-source-key-000000000000"

// This is a synthetic authoritative source database, separate from caller claims.
// Registration happens in test setup, never in the confirmation request handler.
type sourceRegistry struct {
	mu         sync.Mutex
	original   map[uuid.UUID]session.AccessContext
	current    map[session.AccessSubject]session.SourceContext
	coreChecks int
	rejections int
}

func newSourceRegistry() *sourceRegistry {
	return &sourceRegistry{original: map[uuid.UUID]session.AccessContext{}, current: map[session.AccessSubject]session.SourceContext{}}
}
func (s *sourceRegistry) register(a session.AccessContext) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.original[a.InvocationID] = a
	s.current[a.Source.Identity] = *a.Source
}
func (s *sourceRegistry) serve(w http.ResponseWriter, r *http.Request) {
	providerCheck := r.Header.Get("Authorization") == "Bearer "+genericProviderSourceKey
	if !providerCheck && r.Header.Get("Authorization") != "Bearer "+genericSourceKey {
		w.WriteHeader(401)
		return
	}
	var in struct {
		Operation         string          `json:"operation"`
		Context           json.RawMessage `json:"context"`
		ContextDigest     string          `json:"context_digest"`
		CoreContextDigest string          `json:"core_context_digest"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		w.WriteHeader(400)
		return
	}
	var claimed session.AccessContext
	if json.Unmarshal(in.Context, &claimed) != nil || claimed.Source == nil {
		w.WriteHeader(403)
		return
	}
	hash := sha256.Sum256(in.Context)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !providerCheck {
		s.coreChecks++
	}
	original, known := s.original[claimed.InvocationID]
	current, exists := s.current[claimed.Source.Identity]
	expectedOperation := "confirm"
	coreBindingValid := true
	if providerCheck {
		full, _ := json.Marshal(original)
		fullHash := sha256.Sum256(full)
		coreBindingValid = in.CoreContextDigest == hex.EncodeToString(fullHash[:])
		original.AllowedInputSubjects = nil
		expectedOperation = "confirm_provider_projection_v1"
	}
	// Independent source identity/revision/audience plus original author, scope,
	// observation and input authority. A correct digest alone grants nothing.
	same := known && exists && in.Operation == expectedOperation && coreBindingValid && in.ContextDigest == hex.EncodeToString(hash[:]) && reflect.DeepEqual(claimed, original) && reflect.DeepEqual(*claimed.Source, current)
	if !same {
		s.rejections++
	}
	if providerCheck {
		_ = json.NewEncoder(w).Encode(map[string]any{"confirmed": same, "context_digest": in.ContextDigest, "core_context_digest": in.CoreContextDigest, "observed_at": time.Now().UTC()})
	} else {
		_ = json.NewEncoder(w).Encode(servicecredentials.Witness{Confirmed: same, ContextDigest: in.ContextDigest, ObservedAt: time.Now().UTC()})
	}
}
func (s *sourceRegistry) change(a session.AccessContext, kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.current[a.Source.Identity]
	if kind == "revision" {
		current.Revision = "changed-after-admission"
	} else {
		current.Audience = append(append([]session.AccessSubject{}, current.Audience...), session.AccessSubject{Namespace: "users", ID: "new-reader"})
	}
	s.current[a.Source.Identity] = current
}
func (s *sourceRegistry) rejected() int { s.mu.Lock(); defer s.mu.Unlock(); return s.rejections }
func (s *sourceRegistry) count() int    { s.mu.Lock(); defer s.mu.Unlock(); return s.coreChecks }

type genericRig struct {
	t          *testing.T
	store      *store.Store
	box        *remote
	authority  *sourceRegistry
	config     servicecredentials.Config
	coreClient *http.Client
	apiURL     string
	fixtureURL string
}

func genericPost(t *testing.T, endpoint, key string, body any, clients ...*http.Client) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if body == nil {
		raw = nil
	}
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), "POST", endpoint, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Idempotency-Key", uuid.NewString())
	client := &http.Client{Timeout: 10 * time.Second}
	if len(clients) > 0 {
		client = clients[0]
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(response.Body, 65536))
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, answer
}
func newGenericRig(t *testing.T, access session.AccessContext) *genericRig {
	t.Helper()
	targetURL := os.Getenv("ORPHEUS_GENERIC_PROVIDER_TEST_URL")
	if targetURL == "" {
		t.Skip("generic cross-repository runner supplies actual Provider candidate")
	}
	target, err := url.Parse(targetURL)
	if err != nil {
		t.Fatal(err)
	}
	s, box, _, unused := setup(t)
	unused.Disconnect()
	authority := newSourceRegistry()
	authority.register(access)
	mux := http.NewServeMux()
	mux.HandleFunc("/source-confirmation", authority.serve)
	mux.Handle("/", httputil.NewSingleHostReverseProxy(target))
	sourceListener, err := net.Listen("tcp", "0.0.0.0:19182")
	if err != nil {
		t.Fatal(err)
	}
	bridge := httptest.NewUnstartedServer(mux)
	_ = bridge.Listener.Close()
	bridge.Listener = sourceListener
	bridge.StartTLS()
	bridge.URL = "https://127.0.0.1:19182"
	t.Cleanup(bridge.Close)
	endpoint := os.Getenv("ORPHEUS_GENERIC_ENDPOINT")
	if endpoint == "" {
		endpoint = "/api/v1/agent-credentials/generic-v1"
	}
	cfg := servicecredentials.Config{Endpoint: bridge.URL + endpoint, SecretEnv: "GENERIC_PROVIDER_KEY", ContextFields: []string{"subject", "scope_context", "context_observed_at", "source"}, TokenFile: "~/.kord/run.token", MaxLeaseSeconds: 300, MaxRunSeconds: 1200, Confirmation: &servicecredentials.ConfirmationConfig{Endpoint: bridge.URL + "/source-confirmation", SecretEnv: "GENERIC_SOURCE_KEY"}}
	t.Setenv("GENERIC_PROVIDER_KEY", genericProviderKey)
	t.Setenv("GENERIC_SOURCE_KEY", genericSourceKey)
	s.Profiles.Services = map[string]config.Service{"kord": {Name: "Kord", Description: "Opt-in generic candidate", Provider: &cfg}}
	s.CredentialClient = servicecredentials.Client{HTTP: bridge.Client()}
	s.Settings.PublicAPIKeys = []string{genericCoreKey}
	handler, err := httpserver.Handler(s, t.Context())
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "0.0.0.0:19181")
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewUnstartedServer(handler)
	_ = api.Listener.Close()
	api.Listener = listener
	api.StartTLS()
	t.Cleanup(api.Close)
	rig := &genericRig{t: t, store: s, box: box, authority: authority, config: cfg, coreClient: api.Client(), apiURL: "https://127.0.0.1:19181", fixtureURL: targetURL}
	status, raw := genericPost(t, targetURL+"/fixture/reset", genericProviderKey, map[string]any{"access_context": access, "core_callback_origin": os.Getenv("ORPHEUS_GENERIC_CORE_CALLBACK_ORIGIN"), "core_key": genericCoreKey, "source_confirmation_origin": os.Getenv("ORPHEUS_GENERIC_SOURCE_CONFIRMATION_ORIGIN"), "source_key": genericProviderSourceKey, "ca_pem": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw})) + string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: bridge.Certificate().Raw}))})
	if status != 200 {
		t.Fatalf("Provider fixture initialization status=%d response=%s", status, raw)
	}
	deadlineTokenFiles(box)
	return rig
}
func genericAccess(mode string) session.AccessContext {
	a := session.AccessContext{InvocationID: uuid.New(), Subject: session.AccessSubject{Namespace: "users", ID: "5"}, ContextObservedAt: time.Now().UTC(), ScopeContext: session.AccessScopeContext{"agent_id": json.RawMessage(`"cxassist"`), "policy_space_id": json.RawMessage(`"cx-test"`)}, Source: &session.SourceContext{Identity: session.AccessSubject{Namespace: "document", ID: "human-report"}, Revision: "revision-1", Audience: []session.AccessSubject{{Namespace: "users", ID: "5"}, {Namespace: "users", ID: "6"}}}, AllowedInputSubjects: []session.AccessSubject{{Namespace: "users", ID: "5"}}}
	switch mode {
	case "gitlab":
		a.Subject = session.AccessSubject{Namespace: "gitlab", ID: "review-service"}
		a.Source.Identity = session.AccessSubject{Namespace: "gitlab-mr", ID: "project/1/mr/2"}
		a.Source.Audience = []session.AccessSubject{a.Subject}
		a.AllowedInputSubjects = nil
	case "space":
		a.Subject = session.AccessSubject{Namespace: "space", ID: "scheduler-service"}
		a.Source.Identity = session.AccessSubject{Namespace: "space-page", ID: "page/17"}
		a.Source.Audience = []session.AccessSubject{a.Subject}
		a.AllowedInputSubjects = nil
	}
	return a
}
func (r *genericRig) admit(a session.AccessContext) (public.Accepted, int) {
	r.t.Helper()
	body := map[string]any{"allow_multiple_runs": true, "configuration": map[string]any{"agent": map[string]any{"profile": "p"}, "sandbox": map[string]any{"template": "codex", "services": []string{"kord"}}, "limits": map[string]any{"run_timeout_seconds": 600}}, "messages": []map[string]string{{"text": "synthetic protected work"}}, "access_context": a}
	status, raw := genericPost(r.t, r.apiURL+"/api/v1/sessions", genericCoreKey, body, r.coreClient)
	var accepted public.Accepted
	if status == 202 && json.Unmarshal(raw, &accepted) != nil {
		r.t.Fatal("invalid Core acceptance")
	}
	if status != 202 && status != 403 {
		r.t.Fatalf("unexpected Core admission status=%d response=%s", status, raw)
	}
	return accepted, status
}
func (r *genericRig) state(a public.Accepted) servicecredentials.State {
	r.t.Helper()
	state, err := r.store.ServiceCredentials(r.t.Context(), a.SessionID, a.RunID)
	if err != nil {
		r.t.Fatal(err)
	}
	return state
}
func (r *genericRig) authorize(token string) int {
	status, _ := genericPost(r.t, r.fixtureURL+"/authorize", genericProviderKey, map[string]string{"token": token})
	return status
}
func (r *genericRig) revoke(a public.Accepted) {
	r.t.Helper()
	status, raw := genericPost(r.t, r.apiURL+"/api/v1/sessions/"+a.SessionID.String()+"/runs/"+a.RunID.String()+"/cancel", genericCoreKey, nil, r.coreClient)
	if status != 200 && status != 202 {
		r.t.Fatalf("Core cancellation status=%d response=%s", status, raw)
	}
	if err := r.store.RevokeTerminalCredentials(r.t.Context()); err != nil {
		r.t.Fatal(err)
	}
}

func TestGenericProviderA9(t *testing.T) {
	for _, mode := range []string{"human", "gitlab", "space"} {
		t.Run(mode+"/issue-renew-sensitive-restart-cancel-revoke", func(t *testing.T) {
			access := genericAccess(mode)
			r := newGenericRig(t, access)
			accepted, status := r.admit(access)
			if status != 202 {
				t.Fatal("explicit candidate principal policy was denied", status)
			}
			state := r.state(accepted)
			grant := state.Entries["kord"].Grant
			installed := false
			r.box.credentialWrite = func(path string, b []byte) error {
				if bytes.HasSuffix([]byte(path), []byte("/run.token")) {
					installed = string(b) == grant.AccessToken
				}
				return nil
			}
			e := executor(accepted.SessionID, r.store, r.box)
			t.Cleanup(e.Disconnect)
			tick(t, e)
			if !installed || r.box.starts != 1 {
				t.Fatal("grant not installed before worker dispatch")
			}
			before := r.authority.count()
			if r.authorize(grant.AccessToken) != 200 || r.authority.count() <= before {
				t.Fatal("sensitive access did not execute real saved-context callback")
			}
			renewed, err := r.store.CredentialClient.RenewConfirmed(t.Context(), r.config, "kord", access, grant)
			if err != nil {
				t.Fatal("generic renew", err)
			}
			if renewed.AccessToken != grant.AccessToken || renewed.ScopeKey != grant.ScopeKey || !renewed.RunExpiresAt.Equal(grant.RunExpiresAt) {
				t.Fatal("generic renewal changed authority")
			}
			entry := state.Entries["kord"]
			entry.Grant = renewed
			state.Entries["kord"] = entry
			if err = r.store.SaveServiceCredentials(t.Context(), accepted.SessionID, accepted.RunID, state); err != nil {
				t.Fatal(err)
			}
			e.Disconnect()
			e = executor(accepted.SessionID, r.store, r.box)
			t.Cleanup(e.Disconnect)
			tick(t, e)
			if r.box.starts != 1 || r.authorize(grant.AccessToken) != 200 {
				t.Fatal("restart reissued or lost original grant")
			}
			r.revoke(accepted)
			tick(t, e)
			r.box.turns[0].Status = session.Cancelled
			tickUntil(t, e, func() bool {
				record, _, err := r.store.Read(t.Context(), accepted.SessionID)
				return err == nil && !record.SlotReserved
			})
			stopped, err := r.store.Run(t.Context(), accepted.SessionID, accepted.RunID)
			if err != nil || stopped.Status != session.Cancelled || r.box.pauses == 0 {
				t.Fatal("cancel did not stop and settle worker", err)
			}
			if r.authorize(grant.AccessToken) != 403 {
				t.Fatal("cancelled grant authorized")
			}
			if err = r.store.CredentialClient.Revoke(t.Context(), r.config, "kord", access.InvocationID, grant); err != nil {
				t.Fatal("generic revoke replay", err)
			}
			if _, err = r.store.CredentialClient.Renew(t.Context(), r.config, "kord", access.InvocationID, grant); err == nil {
				t.Fatal("revoked grant renewed")
			}
		})
	}
	for _, kind := range []string{"revision", "audience"} {
		t.Run("source-change/"+kind, func(t *testing.T) {
			access := genericAccess("human")
			r := newGenericRig(t, access)
			accepted, status := r.admit(access)
			if status != 202 {
				t.Fatal(status)
			}
			grant := r.state(accepted).Entries["kord"].Grant
			e := executor(accepted.SessionID, r.store, r.box)
			t.Cleanup(e.Disconnect)
			tick(t, e)
			r.authority.change(access, kind)
			if r.authorize(grant.AccessToken) != 403 {
				t.Fatal("sensitive callback accepted changed source")
			}
			if _, err := r.store.CredentialClient.RenewConfirmed(t.Context(), r.config, "kord", access, grant); err == nil {
				t.Fatal("changed source renewed")
			}
			e.Disconnect()
			e = executor(accepted.SessionID, r.store, r.box)
			t.Cleanup(e.Disconnect)
			tick(t, e)
			run, err := r.store.Run(t.Context(), accepted.SessionID, accepted.RunID)
			if err != nil || run.CancelRequestedAt == nil && run.Status != session.Failed {
				t.Fatal("source denial did not stop Core run", err)
			}
			if err = r.store.RevokeTerminalCredentials(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, kind := range []string{"author", "partial-audience", "unknown-source", "input-authority"} {
		t.Run("forged-context/"+kind, func(t *testing.T) {
			original := genericAccess("human")
			r := newGenericRig(t, original)
			raw, _ := json.Marshal(original)
			var forged session.AccessContext
			_ = json.Unmarshal(raw, &forged)
			switch kind {
			case "author":
				forged.Subject.ID = "other"
			case "partial-audience":
				forged.Source.Audience = forged.Source.Audience[:1]
			case "unknown-source":
				forged.Source.Identity.ID = "unregistered"
			case "input-authority":
				forged.AllowedInputSubjects = []session.AccessSubject{{Namespace: "users", ID: "other"}}
			}
			if _, status := r.admit(forged); status != 403 || r.authority.rejected() == 0 {
				t.Fatal("digest echo admitted forged context or source denial not proven", kind, status)
			}
		})
	}
	t.Run("explicit-empty-service-audience-denied-without-fake-human", func(t *testing.T) {
		access := genericAccess("space")
		access.Source.Audience = []session.AccessSubject{}
		r := newGenericRig(t, access)
		if _, status := r.admit(access); status != 403 {
			t.Fatal("missing service-only Provider policy did not deny", status)
		}
	})

	t.Run("current-provider-policy-reduction", func(t *testing.T) {
		access := genericAccess("human")
		r := newGenericRig(t, access)
		accepted, status := r.admit(access)
		if status != 202 {
			t.Fatal(status)
		}
		grant := r.state(accepted).Entries["kord"].Grant
		if r.authorize(grant.AccessToken) != 200 {
			t.Fatal("initial protected access denied")
		}
		if code, _ := genericPost(t, r.fixtureURL+"/fixture/reduce", genericProviderKey, map[string]any{}); code != 200 {
			t.Fatal(code)
		}
		if r.authorize(grant.AccessToken) != 403 {
			t.Fatal("reduced Provider rights retained access")
		}
		if _, err := r.store.CredentialClient.RenewConfirmed(t.Context(), r.config, "kord", access, grant); err == nil {
			t.Fatal("reduced Provider rights renewed")
		}
		r.revoke(accepted)
	})
	for _, change := range []string{"revision", "source", "audience"} {
		t.Run("continuation/"+change, func(t *testing.T) {
			access := genericAccess("human")
			r := newGenericRig(t, access)
			accepted, status := r.admit(access)
			if status != 202 {
				t.Fatal(status)
			}
			e := executor(accepted.SessionID, r.store, r.box)
			t.Cleanup(e.Disconnect)
			tick(t, e)
			complete(r.box)
			tickUntil(t, e, func() bool {
				record, _, err := r.store.Read(t.Context(), accepted.SessionID)
				return err == nil && !record.SlotReserved
			})
			if err := r.store.RevokeTerminalCredentials(t.Context()); err != nil {
				t.Fatal(err)
			}
			saved, _ := json.Marshal(access)
			var next session.AccessContext
			_ = json.Unmarshal(saved, &next)
			next.InvocationID = uuid.New()
			next.ContextObservedAt = time.Now().UTC()
			next.Source.Revision = "revision-2"
			if change == "source" {
				next.Source.Identity.ID = "another-document"
			}
			if change == "audience" {
				next.Source.Audience = next.Source.Audience[:1]
			}
			r.authority.register(next)
			code, raw := genericPost(t, r.apiURL+"/api/v1/sessions/"+accepted.SessionID.String()+"/runs", genericCoreKey, map[string]any{"messages": []map[string]string{{"text": "continue synthetic work"}}, "access_context": next}, r.coreClient)
			if change == "revision" {
				if code != 202 {
					t.Fatalf("compatible revision continuation status=%d body=%s", code, raw)
				}
				var again public.Accepted
				_ = json.Unmarshal(raw, &again)
				r.revoke(again)
			} else if code != 409 || !bytes.Contains(raw, []byte("service_scope_changed")) {
				t.Fatalf("changed boundary inherited memory status=%d body=%s", code, raw)
			}
		})
	}
	t.Run("provider-and-restored-core-expiry", func(t *testing.T) {
		access := genericAccess("human")
		r := newGenericRig(t, access)
		accepted, status := r.admit(access)
		if status != 202 {
			t.Fatal(status)
		}
		state := r.state(accepted)
		grant := state.Entries["kord"].Grant
		e := executor(accepted.SessionID, r.store, r.box)
		t.Cleanup(e.Disconnect)
		tick(t, e)
		e.Disconnect()
		// Explicit fixture clock advance expires the real Provider ledger. The saved
		// Core expiry is moved into its past to model the same elapsed worker outage.
		if code, _ := genericPost(t, r.fixtureURL+"/fixture/advance", genericProviderKey, map[string]int{"seconds": 301}); code != 200 {
			t.Fatal("fixture clock advance", code)
		}
		if r.authorize(grant.AccessToken) != 403 {
			t.Fatal("Provider authorized an expired lease")
		}
		entry := state.Entries["kord"]
		entry.Grant.ExpiresAt = time.Now().Add(-time.Second)
		state.Entries["kord"] = entry
		if err := r.store.SaveServiceCredentials(t.Context(), accepted.SessionID, accepted.RunID, state); err != nil {
			t.Fatal(err)
		}
		e = executor(accepted.SessionID, r.store, r.box)
		t.Cleanup(e.Disconnect)
		tick(t, e)
		run, err := r.store.Run(t.Context(), accepted.SessionID, accepted.RunID)
		if err != nil || run.Status != session.Failed || run.Error == nil || run.Error.Code != "service_credentials_expired" || r.box.starts != 1 {
			t.Fatal("expired restoration resumed work", err)
		}
		if err = r.store.RevokeTerminalCredentials(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}
