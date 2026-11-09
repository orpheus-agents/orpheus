//go:build integration

package worker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus/internal/servicecredentials"
	"github.com/orpheus-agents/orpheus/internal/session"
)

func TestChangedSourceAudienceStopsRunAndSurvivesRestart(t *testing.T) {
	s, box, a, e := setup(t)
	deadlineTokenFiles(box)
	t.Setenv("WORKER_PROVIDER_KEY", strings.Repeat("k", 32))
	var changed atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ContextDigest string                `json:"context_digest"`
			Context       session.AccessContext `json:"context"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			t.Error("invalid source confirmation request")
		}
		if in.Context.Source == nil || in.Context.Source.Identity.Namespace != "space" || in.Context.Subject.Namespace != "service" {
			t.Error("noninteractive source changed")
		}
		_ = json.NewEncoder(w).Encode(servicecredentials.Witness{Confirmed: !changed.Load(), ContextDigest: in.ContextDigest, ObservedAt: time.Now().UTC()})
	}))
	defer server.Close()
	cfg := deadlineProviderConfig()
	cfg.Confirmation = &servicecredentials.ConfirmationConfig{Endpoint: server.URL, SecretEnv: "WORKER_PROVIDER_KEY"}
	now := time.Now().UTC()
	state := workerCredentials(t, s, a, servicecredentials.Grant{CredentialID: "grant", AccessToken: "private", ScopeKey: "scope", ExpiresAt: now.Add(4 * time.Minute), RunExpiresAt: now.Add(15 * time.Minute)}, cfg)
	state.Context.Subject = session.AccessSubject{Namespace: "service", ID: "space-worker"}
	state.Context.Source = &session.SourceContext{Identity: session.AccessSubject{Namespace: "space", ID: "schedule"}, Revision: "occurrence-1", Audience: []session.AccessSubject{}}
	if err := s.SaveServiceCredentials(t.Context(), a.SessionID, a.RunID, state); err != nil {
		t.Fatal(err)
	}
	s.CredentialClient = servicecredentials.Client{HTTP: server.Client()}
	tick(t, e)
	if box.starts != 1 {
		t.Fatal("service-only schedule did not start")
	}
	if err := s.ConfirmInvocation(t.Context(), state.Context.InvocationID); err != nil {
		t.Fatal("saved source did not confirm", err)
	}
	changed.Store(true)
	e.Disconnect()
	e = executor(a.SessionID, s, box)
	t.Cleanup(e.Disconnect)
	tick(t, e)
	run, err := s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || run.Status != session.Failed || run.Error == nil || run.Error.Code != "service_credentials_expired" || box.pauses == 0 || box.starts != 1 {
		t.Fatal("source change did not stop restored run", run, err)
	}
	if err = s.ConfirmInvocation(t.Context(), state.Context.InvocationID); err == nil {
		t.Fatal("terminal invocation confirmed")
	}
}
