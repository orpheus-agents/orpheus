//go:build integration

package worker

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skillum-ai/orpheus/internal/harness"
	"github.com/skillum-ai/orpheus/internal/session"
	"github.com/skillum-ai/orpheus/internal/store"
)

func TestReconnectDuringLoginAndReadyCommit(t *testing.T) {
	for _, phase := range []string{"login", "ready_commit"} {
		t.Run(phase, func(t *testing.T) {
			s, r, a, e := setup(t)
			if phase == "login" {
				r.initializeError = harness.ErrUncertain
			} else {
				_, err := s.Pool.Exec(t.Context(), `CREATE FUNCTION reject_ready() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.kind='launch' AND NEW.result->>'initialized'='true' THEN RAISE EXCEPTION 'fixture'; END IF; RETURN NEW; END $$;
CREATE TRIGGER reject_ready BEFORE UPDATE ON operations FOR EACH ROW EXECUTE FUNCTION reject_ready()`)
				if err != nil {
					t.Fatal(err)
				}
			}
			err := e.Tick(t.Context())
			if err == nil {
				t.Fatal("expected initialization/commit failure")
			}
			if phase == "login" && !errors.Is(err, harness.ErrUncertain) {
				t.Fatal(err)
			}
			r.initializeError = nil
			if phase == "ready_commit" {
				if _, err := s.Pool.Exec(t.Context(), "DROP TRIGGER reject_ready ON operations"); err != nil {
					t.Fatal(err)
				}
			}
			e.Disconnect()
			replacement := executor(a.SessionID, s, r)
			defer replacement.Disconnect()
			tick(t, replacement)
			if r.launches != 1 || r.starts != 1 || r.attaches != 1 {
				t.Fatalf("repeated effects: launch=%d start=%d attach=%d", r.launches, r.starts, r.attaches)
			}
		})
	}
}

func TestCancelUnknownNativeTurnHonorsGrace(t *testing.T) {
	s, r, a, e := setup(t)
	r.startError = harness.ErrUncertain
	if err := e.Tick(t.Context()); !errors.Is(err, harness.ErrUncertain) {
		t.Fatal(err)
	}
	if _, err := s.Cancel(t.Context(), a.SessionID, a.RunID); err != nil {
		t.Fatal(err)
	}
	// No turn ID was acknowledged; expiration must still kill only our process.
	if _, err := s.Pool.Exec(t.Context(), "UPDATE runs SET cancel_attempted_at=now()-interval '60 seconds' WHERE id=$1", a.RunID); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	run, err := s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || run.Status != session.Cancelled || run.StopMethod == nil || *run.StopMethod != "forced" || len(r.killed) != 1 {
		t.Fatal(run, r.killed, err)
	}
}

func TestEnvironmentLossIsAtomicWithRunFailure(t *testing.T) {
	for _, code := range []string{"sandbox_lost", "context_lost"} {
		t.Run(code, func(t *testing.T) {
			s, _, a, e := setup(t)
			tick(t, e)
			// Simulate a failed session update after Finish. Run state must roll back
			// too; otherwise admission could accept a new run in the gap.
			_, err := s.Pool.Exec(t.Context(), `CREATE FUNCTION reject_unavailable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.sandbox_state='unavailable' THEN RAISE EXCEPTION 'fixture'; END IF; RETURN NEW; END $$;
CREATE TRIGGER reject_unavailable BEFORE UPDATE ON sessions FOR EACH ROW EXECUTE FUNCTION reject_unavailable()`)
			if err != nil {
				t.Fatal(err)
			}
			failure := &harness.ExecutionError{Code: code, Message: "Environment unavailable."}
			if e.failure(t.Context(), failure) == nil {
				t.Fatal("expected injected write failure")
			}
			record, run, err := s.Read(t.Context(), a.SessionID)
			if err != nil || run == nil || run.Status != session.Running || !record.SlotReserved || record.SandboxState == "unavailable" {
				t.Fatal("partial environment failure committed", record, run, err)
			}
			if _, err := s.Pool.Exec(t.Context(), "DROP TRIGGER reject_unavailable ON sessions"); err != nil {
				t.Fatal(err)
			}
			if err := e.failure(t.Context(), failure); err != nil {
				t.Fatal(err)
			}
			record, run, err = s.Read(t.Context(), a.SessionID)
			if err != nil || run != nil || record.SandboxState != "unavailable" || (code == "sandbox_lost" && record.SlotReserved) {
				t.Fatal(record, run, err)
			}
			if _, err := s.Accept(t.Context(), store.Admission{SessionID: a.SessionID, Key: uuid.New(), Text: "next"}); err == nil {
				t.Fatal("accepted run in unavailable session")
			}
		})
	}
}

func TestLeaseRenewalIsThrottled(t *testing.T) {
	_, remote, _, e := setup(t)
	tick(t, e)
	initial := remote.renewals
	for range 3 {
		tick(t, e)
	}
	if remote.renewals != initial {
		t.Fatal("renewed timeout on every tick")
	}
	e.timeoutRenewAt = time.Now().Add(-time.Second)
	tick(t, e)
	if remote.renewals != initial+1 {
		t.Fatal("expired renewal deadline ignored")
	}
}
