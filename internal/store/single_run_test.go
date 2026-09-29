//go:build integration

package store

import (
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/session"
)

func TestSingleRunAdmissionAndReplay(t *testing.T) {
	s := fixture(t)
	req := request()
	req.Create.AllowMultipleRuns = false
	a, err := s.Accept(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []session.Status{session.Accepted, session.Starting, session.Running, session.Cancelling, session.Finalizing, session.Completed, session.Failed, session.Cancelled} {
		t.Run(string(status), func(t *testing.T) {
			if err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, record *SessionRecord) error {
				run, err := GetRun(t.Context(), tx, a.SessionID, a.RunID)
				if err != nil {
					return err
				}
				run.Status = status
				if status.Terminal() {
					record.SandboxState = "deleted"
					record.SlotReserved = false
				}
				return PublishRun(t.Context(), tx, record, &run)
			}); err != nil {
				t.Fatal(err)
			}
			before, _, err := s.Read(t.Context(), a.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			errs := make(chan error, 4)
			for range 4 {
				wg.Go(func() {
					_, err := s.Accept(t.Context(), Admission{SessionID: a.SessionID, Key: uuid.New(), Messages: []session.TextMessage{{Text: "next"}}})
					errs <- err
				})
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				requireCode(t, err, "multiple_runs_not_allowed")
			}
			after, _, err := s.Read(t.Context(), a.SessionID)
			if err != nil || before.NextRunNumber != after.NextRunNumber || before.NextEventSequence != after.NextEventSequence || before.SlotReserved != after.SlotReserved {
				t.Fatal("rejection changed session", before, after, err)
			}
			again, err := s.Accept(t.Context(), req)
			if err != nil || again != a {
				t.Fatal("replay changed acceptance", again, err)
			}
			if status == session.Running {
				if _, err := s.Accept(t.Context(), Admission{SessionID: a.SessionID, RunID: a.RunID, Key: uuid.New(), Messages: []session.TextMessage{{Text: "clarification"}}}); err != nil {
					t.Fatal("single-run session rejected steer", err)
				}
			}
		})
	}
	// The policy error takes precedence over budget/environment/capacity errors.
	if err := s.Mutate(t.Context(), a.SessionID, false, func(_ pgx.Tx, record *SessionRecord) error {
		record.TotalTokens = record.Configuration.Public.Limits.MaxSessionTokens
		record.SandboxState = "unavailable"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.Settings.MaxConcurrentSessions = 0
	_, err = s.Accept(t.Context(), Admission{SessionID: a.SessionID, Key: uuid.New(), Messages: []session.TextMessage{{Text: "next"}}})
	requireCode(t, err, "multiple_runs_not_allowed")
	req.Create.AllowMultipleRuns = true
	_, err = s.Accept(t.Context(), req)
	requireCode(t, err, "idempotency_conflict")
}
