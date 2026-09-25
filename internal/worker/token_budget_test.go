//go:build integration

package worker

import (
	"context"
	"encoding/json"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
)

type usageConnection struct {
	harness.Driver
	remote *remote
	closes int
}

func (d *usageConnection) Close() error {
	d.closes++
	d.remote.usage = nil
	return d.Driver.Close()
}

func TestUsageSurvivesDatabaseWriteFailure(t *testing.T) {
	s, box, a, e := setup(t)
	tick(t, e)
	d := &usageConnection{Driver: e.driver, remote: box}
	e.driver = d
	box.usage = []harness.UsageReport{{ContextID: "thread", TurnID: "turn-1", Total: session.Usage{InputTokens: 8, OutputTokens: 2, TotalTokens: 10}}}
	complete(box)
	// Sequences survive rollback: fail exactly the first transaction that
	// saves usage, after both the completed run and its event were written.
	_, err := s.Pool.Exec(t.Context(), `
CREATE SEQUENCE usage_write_attempt;
CREATE FUNCTION reject_first_usage() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.total_tokens > 0 AND nextval('usage_write_attempt') = 1 THEN
    RAISE EXCEPTION 'temporary usage write failure';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER reject_first_usage BEFORE UPDATE ON sessions FOR EACH ROW EXECUTE FUNCTION reject_first_usage();`)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	e.Run(ctx)
	if ctx.Err() != nil {
		t.Fatal(ctx.Err())
	}
	run, err := s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || run.Status != session.Completed || run.Usage.TotalTokens != 10 || d.closes != 1 {
		t.Fatal("usage was lost or connection replaced after database failure", run, d.closes, err)
	}
	var attempts int
	if err := s.Pool.QueryRow(t.Context(), "SELECT last_value FROM usage_write_attempt").Scan(&attempts); err != nil || attempts < 2 {
		t.Fatal("database retry was not exercised", attempts, err)
	}
}

func capReport(turn string) harness.UsageReport {
	return harness.UsageReport{ContextID: "thread", TurnID: turn, Total: session.Usage{InputTokens: config.DefaultMaxSessionTokens - 1, OutputTokens: 1, TotalTokens: config.DefaultMaxSessionTokens}}
}
func TestTokenBudgetCancellationAndFinalHook(t *testing.T) {
	for _, forced := range []bool{false, true} {
		t.Run(map[bool]string{false: "graceful", true: "forced"}[forced], func(t *testing.T) {
			s, box, a, e := setupHooks(t, true, false)
			tickUntil(t, e, func() bool { return box.starts == 1 })
			box.usage = []harness.UsageReport{capReport("turn-1")}
			tick(t, e)
			run, err := s.Run(t.Context(), a.SessionID, a.RunID)
			if err != nil || run.Status != session.Cancelling || run.StopReason == nil || *run.StopReason != "token_limit" || box.cancels != 1 {
				t.Fatal(run, box.cancels, err)
			}
			tick(t, e)
			if box.cancels != 1 {
				t.Fatal("interrupt repeated", box.cancels)
			}
			if forced {
				if _, err := s.Pool.Exec(t.Context(), `UPDATE runs SET cancel_attempted_at=now()-interval '1 minute' WHERE id=$1`, a.RunID); err != nil {
					t.Fatal(err)
				}
			} else {
				box.turns[0].Status = session.Cancelled
			}
			tickUntil(t, e, func() bool {
				r, err := s.Run(t.Context(), a.SessionID, a.RunID)
				return err == nil && r.Status.Terminal()
			})
			run, err = s.Run(t.Context(), a.SessionID, a.RunID)
			method := "graceful"
			if forced {
				method = "forced"
			}
			if err != nil || run.Status != session.Cancelled || run.StopMethod == nil || *run.StopMethod != method || run.Usage.TotalTokens != config.DefaultMaxSessionTokens || run.Hooks[2].Status != "completed" {
				t.Fatal(run, err)
			}
			env := box.hookEnv[len(box.hookEnv)-1]
			if env["ORPHEUS_STOP_REASON"] != "token_limit" || env["ORPHEUS_AGENT_STATUS"] != "cancelled" {
				t.Fatal(env)
			}
			tick(t, e)
			view, err := s.Session(t.Context(), a.SessionID)
			if err != nil || view.Sandbox.State != "paused" || view.Usage != run.Usage {
				t.Fatal(view, err)
			}
			_, err = s.Accept(t.Context(), store.Admission{SessionID: a.SessionID, Key: uuid.New(), Messages: []session.TextMessage{{Text: "next"}}})
			if err == nil || err.Error() != "token_limit_exceeded" {
				t.Fatal(err)
			}
		})
	}
}
func TestTokenBudgetDoesNotCancelCompletedAgent(t *testing.T) {
	s, box, a, e := setupHooks(t, true, false)
	tickUntil(t, e, func() bool { return box.starts == 1 })
	box.usage = []harness.UsageReport{capReport("turn-1")}
	complete(box.remote)
	tickUntil(t, e, func() bool {
		r, err := s.Run(t.Context(), a.SessionID, a.RunID)
		return err == nil && r.Status.Terminal()
	})
	r, err := s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || r.Status != session.Completed || r.CancelRequestedAt != nil || r.Usage.TotalTokens != config.DefaultMaxSessionTokens || box.cancels != 0 || r.Hooks[2].Status != "completed" {
		t.Fatal(r, err)
	}
}
func TestTokenBudgetBeforePreparationAndDispatch(t *testing.T) {
	for _, dispatch := range []bool{false, true} {
		t.Run(map[bool]string{false: "preparation", true: "dispatch"}[dispatch], func(t *testing.T) {
			s, box, a, e := setup(t)
			old, run, err := s.Read(t.Context(), a.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Mutate(t.Context(), a.SessionID, false, func(_ pgx.Tx, r *store.SessionRecord) error {
				r.TotalTokens = config.DefaultMaxSessionTokens
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if dispatch {
				if err := e.send(t.Context(), old, *run); err != nil {
					t.Fatal(err)
				}
			}
			tick(t, e)
			got, err := s.Run(t.Context(), a.SessionID, a.RunID)
			if err != nil || got.Status != session.Cancelled || got.ExecutionStartedAt != nil || got.StopMethod != nil || got.StopReason == nil || *got.StopReason != "token_limit" || box.starts != 0 || box.creates != 0 {
				t.Fatal(got, box.starts, err)
			}
		})
	}
}
func TestTokenBudgetStopsPreparationHook(t *testing.T) {
	s, box, a, e := setupHooks(t, false, false)
	tickUntil(t, e, func() bool { return len(box.invoked) == 1 })
	h, err := s.Hook(t.Context(), a.RunID, "after_create")
	if err != nil {
		t.Fatal(err)
	}
	dir := path.Join("/home/template/.orpheus/hooks", h.ID.String())
	started, _ := json.Marshal(hookStarted{OperationID: h.ID.String(), WrapperPID: 200, HookPID: new(201)})
	box.files[path.Join(dir, "started.json")] = started
	box.processes = append(box.processes, harness.Process{PID: 200, Env: map[string]string{"ORPHEUS_HOOK_OPERATION_ID": h.ID.String()}})
	if err := s.Mutate(t.Context(), a.SessionID, false, func(_ pgx.Tx, r *store.SessionRecord) error {
		r.TotalTokens = config.DefaultMaxSessionTokens
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	if len(box.signals) != 1 || !strings.Contains(box.signals[0], "-TERM 201") {
		t.Fatal(box.signals)
	}
	result, _ := json.Marshal(hookResultFile{OperationID: h.ID.String(), FinishedAt: time.Now().UTC(), Signal: new(15), OutputCompleteness: "complete", HeadFile: "head.txt"})
	box.files[path.Join(dir, "result.json")] = result
	box.files[path.Join(dir, "head.txt")] = nil
	box.processes = nil
	tickUntil(t, e, func() bool {
		r, err := s.Run(t.Context(), a.SessionID, a.RunID)
		return err == nil && r.Status.Terminal()
	})
	r, err := s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || r.Status != session.Cancelled || r.StopMethod != nil || r.Hooks[0].Status != "cancelled" || r.Hooks[2].Status != "skipped" || box.starts != 0 {
		t.Fatal(r, err)
	}
}
