//go:build integration

package store

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/servicecredentials"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

func protectedFixture(t *testing.T) (*Store, Admission, *atomic.Int32, *atomic.Bool, *atomic.Bool) {
	t.Helper()
	s := fixture(t)
	t.Setenv("PROVIDER_TEST_KEY", strings.Repeat("x", 32))
	var calls atomic.Int32
	var unavailable, changed atomic.Bool
	expires := time.Now().UTC().Add(290 * time.Second)
	absolute := time.Now().UTC().Add(1190 * time.Second)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if unavailable.Load() {
			http.Error(w, "private-provider-error", http.StatusServiceUnavailable)
			return
		}
		var in struct {
			Operation    string    `json:"operation"`
			InvocationID uuid.UUID `json:"invocation_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
		}
		scope := "common"
		if changed.Load() {
			scope = "narrower"
		}
		_ = json.NewEncoder(w).Encode(servicecredentials.Grant{CredentialID: in.InvocationID.String(), AccessToken: "protected-private-token", ScopeKey: scope, ExpiresAt: expires, RunExpiresAt: absolute})
	}))
	t.Cleanup(server.Close)
	s.CredentialClient = servicecredentials.Client{HTTP: server.Client()}
	s.Profiles.Services = map[string]config.Service{"kord": {Name: "Kord", Description: "Protected source", Provider: &servicecredentials.Config{Endpoint: server.URL, SecretEnv: "PROVIDER_TEST_KEY", TokenFile: "~/.kord/run.token", ContextFields: []string{"subject", "scope_context", "context_observed_at"}}}}
	req := request()
	req.Create.Configuration.Sandbox.Services = []string{"kord"}
	req.Create.AccessContext = &session.AccessContext{InvocationID: uuid.New(), Subject: session.AccessSubject{Namespace: "scheduler", ID: "nightly"}, ScopeContext: session.AccessScopeContext{"schedule": json.RawMessage(`"nightly"`), "workspace": json.RawMessage(`"support"`)}, AllowedInputSubjects: []session.AccessSubject{{Namespace: "users", ID: "bob"}}, ContextObservedAt: time.Now().UTC()}
	return s, req, &calls, &unavailable, &changed
}
func TestProtectedAdmissionReplayScopeAndEncryption(t *testing.T) {
	s, req, calls, unavailable, changed := protectedFixture(t)
	accepted, err := s.Accept(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	changed.Store(true)
	busyContext := *req.Create.AccessContext
	busyContext.InvocationID = uuid.New()
	_, busyErr := s.Accept(t.Context(), Admission{Key: uuid.New(), SessionID: accepted.SessionID, Messages: []session.TextMessage{{Text: "busy"}}, AccessContext: &busyContext})
	requireCode(t, busyErr, "session_busy")
	changed.Store(false)
	row, err := db.New(s.Pool).GetServiceCredentials(t.Context(), accepted.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row.Ciphertext, "protected-private-token") || strings.Contains(string(row.AccessContext), "protected-private-token") || strings.Contains(string(row.Scopes), "protected-private-token") {
		t.Fatal("plaintext credential persisted")
	}
	state, err := s.ServiceCredentials(t.Context(), accepted.SessionID, accepted.RunID)
	if err != nil || state.Entries["kord"].Grant.AccessToken != "protected-private-token" {
		t.Fatal("restart lost credentials", err)
	}
	if _, err := s.ServiceCredentials(t.Context(), uuid.New(), accepted.RunID); err == nil {
		t.Fatal("ciphertext not bound to session")
	}
	unavailable.Store(true)
	before := calls.Load()
	again, err := s.Accept(t.Context(), req)
	if err != nil || again != accepted || calls.Load() != before {
		t.Fatal("replay called provider", err)
	}
	copyContext := *req.Create.AccessContext
	copyContext.Subject.ID = "bob"
	req.Create.AccessContext = &copyContext
	_, err = s.Accept(t.Context(), req)
	requireCode(t, err, "idempotency_conflict")
	unavailable.Store(false)
	if _, err = s.Cancel(t.Context(), accepted.SessionID, accepted.RunID); err != nil {
		t.Fatal(err)
	}
	copyContext.InvocationID = uuid.New()
	next := Admission{Key: uuid.New(), SessionID: accepted.SessionID, Messages: []session.TextMessage{{Text: "continue as Bob"}}, AccessContext: &copyContext}
	second, err := s.Accept(t.Context(), next)
	if err != nil || second.SessionID != accepted.SessionID {
		t.Fatal("author change reset same scope", err)
	}
	if _, err = s.Cancel(t.Context(), second.SessionID, second.RunID); err != nil {
		t.Fatal(err)
	}
	changed.Store(true)
	copyContext.InvocationID = uuid.New()
	next.Key = uuid.New()
	_, err = s.Accept(t.Context(), next)
	requireCode(t, err, "service_scope_changed")
	req.Key = uuid.New()
	req.Create.AccessContext = &copyContext
	req.Create.Messages = []session.TextMessage{{Text: "clean trigger only"}}
	clean, err := s.Accept(t.Context(), req)
	if err != nil || clean.SessionID == accepted.SessionID {
		t.Fatal("same invocation clean session failed", err)
	}
	// Re-keying an accepted invocation must not execute it a second time.
	req.Key = uuid.New()
	_, err = s.Accept(t.Context(), req)
	requireCode(t, err, "invocation_already_accepted")
	events, err := s.Events(t.Context(), accepted.SessionID, "0", 100)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(events)
	if strings.Contains(string(raw), "protected-private-token") {
		t.Fatal("credential in public events")
	}
}
func TestProtectedActiveInputAuthorsAndServiceRemoval(t *testing.T) {
	s, req, calls, _, _ := protectedFixture(t)
	a, err := s.Accept(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, r *SessionRecord) error {
		run, err := GetRun(t.Context(), tx, a.SessionID, a.RunID)
		if err != nil {
			return err
		}
		run.Status = session.Running
		return PublishRun(t.Context(), tx, r, &run)
	}); err != nil {
		t.Fatal(err)
	}
	input := Admission{Key: uuid.New(), SessionID: a.SessionID, RunID: a.RunID, Messages: []session.TextMessage{{Text: "clarify"}}}
	before := calls.Load()
	_, err = s.Accept(t.Context(), input)
	requireCode(t, err, "access_context_required")
	input.InputSubjects = []session.AccessSubject{{Namespace: "users", ID: "outsider"}}
	_, err = s.Accept(t.Context(), input)
	requireCode(t, err, "input_subject_denied")
	input.InputSubjects = []session.AccessSubject{{Namespace: "other", ID: "bob"}}
	_, err = s.Accept(t.Context(), input)
	requireCode(t, err, "input_subject_denied")
	input.InputSubjects = []session.AccessSubject{{Namespace: "users", ID: "bob"}}
	msg, err := s.Accept(t.Context(), input)
	if err != nil || calls.Load() != before {
		t.Fatal("steer reissued", err)
	}
	var subject []byte
	if err = s.Pool.QueryRow(t.Context(), "SELECT subject FROM run_input_subjects WHERE message_id=$1", msg.MessageID).Scan(&subject); err != nil || !strings.Contains(string(subject), "bob") {
		t.Fatal("no input audit", err)
	}
	if err = s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, r *SessionRecord) error {
		run, err := GetRun(t.Context(), tx, a.SessionID, a.RunID)
		if err != nil {
			return err
		}
		return Finish(t.Context(), tx, r, &run, session.Completed, nil, nil)
	}); err != nil {
		t.Fatal(err)
	}
	delete(s.Profiles.Services, "kord")
	_, err = s.Accept(t.Context(), Admission{Key: uuid.New(), SessionID: a.SessionID, Messages: []session.TextMessage{{Text: "continue"}}})
	requireCode(t, err, "service_scope_changed")
}
func TestProtectedConcurrentAdmissionAndCapacity(t *testing.T) {
	s, req, _, _, _ := protectedFixture(t)
	var wg sync.WaitGroup
	results := make(chan session.Acceptance, 8)
	for range 8 {
		wg.Go(func() {
			a, err := s.Accept(t.Context(), req)
			if err != nil {
				t.Error(err)
				return
			}
			results <- a
		})
	}
	wg.Wait()
	close(results)
	var first session.Acceptance
	for a := range results {
		if first.SessionID == uuid.Nil {
			first = a
		}
		if first != a {
			t.Fatal("duplicate accepted run")
		}
	}
	var count int
	if err := s.Pool.QueryRow(t.Context(), "SELECT count(*) FROM run_service_credentials").Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate durable grant", count, err)
	}
	req.Key = uuid.New()
	req.Create.AccessContext.InvocationID = uuid.New()
	s.Settings.MaxConcurrentSessions = 1
	_, err := s.Accept(t.Context(), req)
	requireCode(t, err, "capacity_exhausted")
}
func TestLegacySessionCannotAcquireProtectedMemory(t *testing.T) {
	s, req, _, _, _ := protectedFixture(t)
	provider := s.Profiles.Services["kord"]
	s.Profiles.Services["kord"] = config.Service{Name: "Kord", Description: "Legacy", EnvFrom: []string{"LEGACY"}}
	req.Create.Configuration.Sandbox.EnvFrom = nil
	a, err := s.Accept(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Cancel(t.Context(), a.SessionID, a.RunID); err != nil {
		t.Fatal(err)
	}
	s.Profiles.Services["kord"] = provider
	_, err = s.Accept(t.Context(), Admission{Key: uuid.New(), SessionID: a.SessionID, Messages: []session.TextMessage{{Text: "new protected query"}}, AccessContext: req.Create.AccessContext})
	requireCode(t, err, "service_scope_changed")
}

func TestProtectedNoInputAllowlistDeniesLegacyAudienceAndInitiator(t *testing.T) {
	for _, allowed := range [][]session.AccessSubject{nil, {}} {
		s, req, calls, _, _ := protectedFixture(t)
		req.Create.AccessContext.Subject = session.AccessSubject{Namespace: "users", ID: "bob"}
		req.Create.AccessContext.ScopeContext = session.AccessScopeContext{"audience_subjects": json.RawMessage(`["bob"]`)}
		req.Create.AccessContext.AllowedInputSubjects = allowed
		a, err := s.Accept(t.Context(), req)
		if err != nil {
			t.Fatal("noninteractive invocation rejected", err)
		}
		if err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, r *SessionRecord) error {
			run, err := GetRun(t.Context(), tx, a.SessionID, a.RunID)
			if err != nil {
				return err
			}
			run.Status = session.Running
			return PublishRun(t.Context(), tx, r, &run)
		}); err != nil {
			t.Fatal(err)
		}
		before := calls.Load()
		_, err = s.Accept(t.Context(), Admission{Key: uuid.New(), SessionID: a.SessionID, RunID: a.RunID, Messages: []session.TextMessage{{Text: "clarify"}}, InputSubjects: []session.AccessSubject{{Namespace: "users", ID: "bob"}}})
		requireCode(t, err, "input_subject_denied")
		if calls.Load() != before {
			t.Fatal("input validation issued credentials")
		}
	}
}

func TestProtectedProviderProjectionCompatibility(t *testing.T) {
	s, req, _, _, _ := protectedFixture(t)
	a, err := s.Accept(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cancel(t.Context(), a.SessionID, a.RunID); err != nil {
		t.Fatal(err)
	}
	provider := s.Profiles.Services["kord"].Provider
	provider.ContextFields = []string{"context_observed_at", "subject", "scope_context"}
	next := *req.Create.AccessContext
	next.InvocationID = uuid.New()
	admission := Admission{Key: uuid.New(), SessionID: a.SessionID, Messages: []session.TextMessage{{Text: "continue"}}, AccessContext: &next}
	second, err := s.Accept(t.Context(), admission)
	if err != nil || second.SessionID != a.SessionID {
		t.Fatal("projection order reset compatible session", err)
	}
	if _, err := s.Cancel(t.Context(), second.SessionID, second.RunID); err != nil {
		t.Fatal(err)
	}
	provider.ContextFields = []string{"subject"}
	next.InvocationID = uuid.New()
	admission.Key = uuid.New()
	_, err = s.Accept(t.Context(), admission)
	requireCode(t, err, "service_scope_changed")
}
