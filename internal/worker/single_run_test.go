//go:build integration

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
)

func TestSingleRunCleanupOutcomes(t *testing.T) {
	for _, outcome := range []string{"completed", "failed", "cancelled", "timeout", "token_limit", "cancel_before_create", "create_rejected", "prepare_rejected", "context_lost", "sandbox_lost", "lost_create_cancel"} {
		t.Run(outcome, func(t *testing.T) {
			s, box, a, e := setupSession(t, false, session.SandboxInput{Template: "codex"}, nil)
			wantStatus, wantSandbox, wantDeletes := session.Failed, "deleted", 1
			switch outcome {
			case "cancel_before_create":
				if _, err := s.Cancel(t.Context(), a.SessionID, a.RunID); err != nil {
					t.Fatal(err)
				}
				wantStatus, wantSandbox, wantDeletes = session.Cancelled, "not_created", 0
			case "create_rejected":
				box.createError = harness.ErrEnvironmentRejected
				wantSandbox, wantDeletes = "provisioning", 0
			case "prepare_rejected":
				box.prepareError = harness.ErrEnvironmentRejected
			case "lost_create_cancel":
				box.lostCreate = true
				if err := e.Tick(t.Context()); !errors.Is(err, harness.ErrUncertain) {
					t.Fatal(err)
				}
				if _, err := s.Cancel(t.Context(), a.SessionID, a.RunID); err != nil {
					t.Fatal(err)
				}
				wantStatus = session.Cancelled
			default:
				tick(t, e)
				switch outcome {
				case "completed":
					complete(box)
					box.usage = []harness.UsageReport{{ContextID: "thread", TurnID: "turn-1", Total: session.Usage{InputTokens: 8, OutputTokens: 2, TotalTokens: 10}}}
					wantStatus = session.Completed
				case "failed":
					box.turns[0].Status = session.Failed
				case "context_lost":
					box.snapshotError = harness.Failure("context_lost", "Context lost.")
				case "sandbox_lost":
					box.state = "lost"
					box.snapshotError = harness.ErrNotFound
					wantSandbox, wantDeletes = "unavailable", 0
				case "cancelled", "timeout", "token_limit":
					wantStatus = session.Cancelled
					if outcome == "cancelled" {
						if _, err := s.Cancel(t.Context(), a.SessionID, a.RunID); err != nil {
							t.Fatal(err)
						}
					} else if err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, record *store.SessionRecord) error {
						run, err := store.GetRun(t.Context(), tx, a.SessionID, a.RunID)
						if err != nil {
							return err
						}
						if outcome == "token_limit" {
							record.TotalTokens = record.Configuration.Public.Limits.MaxSessionTokens
						} else {
							run.DeadlineAt = new(time.Now().Add(-time.Minute))
						}
						return store.SaveRun(t.Context(), tx, &run)
					}); err != nil {
						t.Fatal(err)
					}
					tick(t, e)
					box.turns[0].Status = session.Cancelled
				}
			}
			tickUntil(t, e, func() bool {
				record, _, err := s.Read(t.Context(), a.SessionID)
				return err == nil && !record.SlotReserved
			})
			view, err := s.Session(t.Context(), a.SessionID)
			if err != nil || view.Status != wantStatus || view.Sandbox.State != wantSandbox || box.deletes != wantDeletes || box.pauses != 0 || view.AllowMultipleRuns {
				t.Fatal(view, box.deletes, box.pauses, err)
			}
			if wantSandbox == "deleted" && (view.Sandbox.ID == nil || view.Sandbox.Workspace == nil && outcome == "completed" || view.Sandbox.LastKnownState == nil || *view.Sandbox.LastKnownState != "deleted") {
				t.Fatal("cleanup lost diagnostic address", view.Sandbox)
			}
			if outcome == "completed" && (view.FinalMessage == nil || view.FinalMessage.Text != "DONE" || view.Usage.TotalTokens != 10) {
				t.Fatal("cleanup lost result or usage", view)
			}
			if box.creates > 1 || outcome == "lost_create_cancel" && box.starts != 0 {
				t.Fatal("cleanup created replacement or started agent", box.creates, box.starts)
			}
			_, err = s.Accept(t.Context(), store.Admission{SessionID: a.SessionID, Key: uuid.New(), Messages: []session.TextMessage{{Text: "retry"}}})
			if err == nil || err.Error() != "multiple_runs_not_allowed" {
				t.Fatal(err)
			}
		})
	}
}

func TestSingleRunDeleteRecovery(t *testing.T) {
	for _, fault := range []string{"offline", "lost_response", "paused", "not_found", "confirmed_before_state", "database_after_delete"} {
		t.Run(fault, func(t *testing.T) {
			s, box, a, e := setupSession(t, false, session.SandboxInput{Template: "codex"}, nil)
			tick(t, e)
			complete(box)
			tick(t, e)
			before, err := s.Run(t.Context(), a.SessionID, a.RunID)
			if err != nil {
				t.Fatal(err)
			}
			unregistered := false
			e.unregisterLimits = func() { unregistered = true }
			box.deleteHook = func() {
				if !unregistered {
					t.Fatal("account limits donor still registered")
				}
			}
			switch fault {
			case "offline":
				box.deleteError = errors.New("offline")
			case "lost_response":
				box.lostDelete = true
			case "paused":
				e.Disconnect()
				box.state = "paused"
			case "not_found":
				box.state = "lost" // The cached handle must also tolerate Delete's NotFound.
			case "confirmed_before_state":
				op, err := s.Operation(t.Context(), a.SessionID, "sandbox_delete", &a.RunID, nil, "sandbox")
				if err != nil {
					t.Fatal(err)
				}
				if err := s.SetOperation(t.Context(), a.SessionID, op.ID, "confirmed", nil); err != nil {
					t.Fatal(err)
				}
				box.state = "lost"
			case "database_after_delete":
				if _, err := s.Pool.Exec(t.Context(), `
CREATE FUNCTION reject_deleted() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.sandbox_state = 'deleted' THEN RAISE EXCEPTION 'database unavailable'; END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER reject_deleted BEFORE UPDATE ON sessions FOR EACH ROW EXECUTE FUNCTION reject_deleted();`); err != nil {
					t.Fatal(err)
				}
			}
			tick(t, e)
			if fault == "offline" || fault == "lost_response" || fault == "database_after_delete" {
				record, _, err := s.Read(t.Context(), a.SessionID)
				if err != nil || !record.SlotReserved || record.SandboxState != "deleting" || record.SandboxError == nil || record.SandboxError.Code != "sandbox_delete_failed" {
					t.Fatal(record, err)
				}
				tick(t, e)
				if box.deletes != 1 {
					t.Fatal("delete did not back off", box.deletes)
				}
				if fault == "database_after_delete" {
					if _, err := s.Pool.Exec(t.Context(), "DROP TRIGGER reject_deleted ON sessions"); err != nil {
						t.Fatal(err)
					}
				}
				e.Disconnect()
				e = executor(a.SessionID, s, box)
				t.Cleanup(e.Disconnect)
				box.deleteError = nil
				tick(t, e)
			}
			after, err := s.Run(t.Context(), a.SessionID, a.RunID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("cleanup changed terminal result", before, after, err)
			}
			record, _, err := s.Read(t.Context(), a.SessionID)
			if err != nil || record.SlotReserved || record.SandboxState != "deleted" || record.SandboxError != nil || box.pauses != 0 {
				t.Fatal(record, err)
			}
			if fault == "paused" && box.resumes != 0 {
				t.Fatal("deletion resumed paused sandbox")
			}
			op, err := s.LatestAttempt(t.Context(), a.SessionID, "sandbox_delete")
			if err != nil || op == nil || op.Status != "confirmed" {
				t.Fatal("delete confirmation lost", op, err)
			}
			events, err := s.Events(t.Context(), a.SessionID, "0", 100)
			if err != nil {
				t.Fatal(err)
			}
			last := events.Items[len(events.Items)-1]
			var snapshot session.SandboxState
			if err := json.Unmarshal(last.Data, &snapshot); err != nil || last.Type != "sandbox.updated" || snapshot.State != "deleted" || snapshot.ID == nil {
				t.Fatal("missing deleted event", last, err)
			}
			deletes := box.deletes
			tick(t, e)
			if box.deletes != deletes {
				t.Fatal("confirmed deletion was repeated", box.deletes)
			}
		})
	}
}

func TestSingleRunHooksBeforeDelete(t *testing.T) {
	for _, failAfter := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "hook_failure"}[failAfter], func(t *testing.T) {
			s, box, a, e := setupHooksSession(t, false, true, failAfter)
			tickUntil(t, e, func() bool { return box.starts == 1 })
			complete(box.remote)
			box.deleteHook = func() {
				run, err := s.Run(t.Context(), a.SessionID, a.RunID)
				if err != nil || !run.Status.Terminal() || len(run.Hooks) != 3 || run.FinalMessage == nil || run.Hooks[2].Status == "pending" || run.Hooks[2].Status == "running" {
					t.Fatal("deleted before result/hooks were saved", run, err)
				}
			}
			tickUntil(t, e, func() bool { return box.deletes == 1 })
			run, err := s.Run(t.Context(), a.SessionID, a.RunID)
			want := session.Completed
			if failAfter {
				want = session.Failed
			}
			if err != nil || run.Status != want || len(box.invoked) != 3 {
				t.Fatal(run, box.invoked, err)
			}
			for _, script := range box.invoked {
				if strings.Contains(script, "before_remove") {
					t.Fatal("activated before_remove")
				}
			}
		})
	}
}

func TestSingleRunRecoveryDuringRun(t *testing.T) {
	s, box, a, e := setupSession(t, false, session.SandboxInput{Template: "codex"}, nil)
	tick(t, e)
	e.Disconnect()
	box.state = "paused" // Lease expiration while the worker was down.
	e = executor(a.SessionID, s, box)
	t.Cleanup(e.Disconnect)
	tick(t, e)
	if box.starts != 1 || box.creates != 1 || box.resumes != 1 || box.deletes != 0 {
		t.Fatal("recovery replaced/deleted active run", box)
	}
	if _, err := s.Accept(t.Context(), store.Admission{SessionID: a.SessionID, RunID: a.RunID, Key: uuid.New(), Messages: []session.TextMessage{{Text: "continue"}}}); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	if box.steers != 1 {
		t.Fatal("clarification was not delivered")
	}
	complete(box)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	e.Run(ctx)
	if ctx.Err() != nil || box.deletes != 1 {
		t.Fatal(ctx.Err(), box.deletes)
	}
}
