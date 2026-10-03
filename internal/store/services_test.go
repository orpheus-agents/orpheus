//go:build integration

package store

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/session"
)

func TestServiceAdmissionSnapshotsAndReplay(t *testing.T) {
	s := fixture(t)
	s.Profiles.Services = map[string]config.Service{
		"a": {Name: "First", Description: "First service", EnvFrom: []string{"A", "SHARED"}},
		"b": {Name: "Second", Description: "Second service", EnvFrom: []string{"B", "SHARED"}},
	}
	req := request()
	req.Create.Configuration.Sandbox.Services = []string{"b", "a"}
	req.Create.Configuration.Sandbox.EnvFrom = []string{"SHARED"}
	req.Create.Services = []string{"b", "a"}
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
	var accepted session.Acceptance
	for a := range results {
		if accepted.SessionID == uuid.Nil {
			accepted = a
		}
		if a != accepted {
			t.Fatal("concurrent replay split", accepted, a)
		}
	}
	if accepted.SessionID == uuid.Nil {
		t.Fatal("no accepted request")
	}
	get := func() {
		t.Helper()
		r := mustSession(t, s, accepted.SessionID)
		if len(r.Configuration.Public.Sandbox.Services) != 2 || r.Configuration.Public.Sandbox.Services[0].Name != "First" || !slices.Equal(r.Configuration.Public.Sandbox.EnvFrom, []string{"A", "B", "SHARED"}) {
			t.Fatal(r.Configuration)
		}
		run, err := s.Run(t.Context(), accepted.SessionID, accepted.RunID)
		if err != nil || len(run.Services) != 2 || run.Services[0].Name != "First" || !slices.Equal(run.EnvFrom, []string{"A", "B", "SHARED"}) {
			t.Fatal(run, err)
		}
	}
	get()
	req.Create.Services = []string{"a", "b"}
	req.Create.Configuration.Sandbox.Services = []string{"a", "b"}
	s.Profiles.Services["a"] = config.Service{Name: "Changed", EnvFrom: []string{"NEW"}}
	delete(s.Profiles.Services, "b")
	// Simulate a fresh process with a different catalog and the same storage.
	restarted := *s
	again, err := restarted.Accept(t.Context(), req)
	if err != nil || again != accepted {
		t.Fatal("catalog-dependent replay", again, err)
	}
	get()
	req.Create.Services = []string{"a"}
	_, err = s.Accept(t.Context(), req)
	requireCode(t, err, "idempotency_conflict")
	req.Create.Services = []string{"a", "b"}
	req.Create.Configuration.Sandbox.Services = []string{"a"}
	_, err = s.Accept(t.Context(), req)
	requireCode(t, err, "idempotency_conflict")
	req.Key = uuid.New()
	_, err = s.Accept(t.Context(), req)
	requireCode(t, err, "unknown_service")
	events, err := s.Events(t.Context(), accepted.SessionID, "0", 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events.Items {
		if strings.Contains(string(event.Data), "private") {
			t.Fatal("secret in event")
		}
		if event.Type == "run.updated" {
			var run session.Run
			if err := json.Unmarshal(event.Data, &run); err != nil || len(run.Services) != 2 {
				t.Fatal(run, err)
			}
		}
	}
	if _, err := s.Cancel(t.Context(), accepted.SessionID, accepted.RunID); err != nil {
		t.Fatal(err)
	}
	nextReq := Admission{SessionID: accepted.SessionID, Key: uuid.New(), Messages: []session.TextMessage{{Text: "next"}}, Services: []string{"a"}}
	next, err := s.Accept(t.Context(), nextReq)
	if err != nil {
		t.Fatal(err)
	}
	nextRun, err := s.Run(t.Context(), next.SessionID, next.RunID)
	if err != nil || len(nextRun.Services) != 1 || nextRun.Services[0].Name != "Changed" || !slices.Equal(nextRun.EnvFrom, []string{"NEW"}) {
		t.Fatal(nextRun, err)
	}
	clear(s.Profiles.Services)
	if got, err := s.Accept(t.Context(), nextReq); err != nil || got != next {
		t.Fatal(got, err)
	}
	nextReq.Services = nil
	_, err = s.Accept(t.Context(), nextReq)
	requireCode(t, err, "idempotency_conflict")
}

func TestEmptyServicesPreserveLegacyFingerprint(t *testing.T) {
	// Historical input with no service fields; adding empty arrays must not
	// change its idempotency digest. Non-empty selection must change it.
	a := request()
	a.Messages = a.Create.Messages
	before, err := fingerprint(a)
	const legacy = "f9fc0d8988388e8b3413e42bd7b248d7e4a0cfded930b777dccaea7e6580ddb7"
	if err != nil || before != legacy {
		t.Fatal("legacy request fingerprint changed", before, err)
	}
	a.Services = []string{}
	a.Create.Configuration.Sandbox.Services = []string{}
	after, err := fingerprint(a)
	if err != nil || before != after {
		t.Fatal(before, after, err)
	}
	a.Services = []string{"a"}
	changed, err := fingerprint(a)
	if err != nil || changed == before {
		t.Fatal(changed, err)
	}
}
