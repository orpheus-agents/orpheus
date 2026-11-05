package worker

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

const sandboxCleanupInterval = 4 * time.Hour
const sandboxCleanupConcurrency = 3

type retentionDeletion struct {
	SandboxID string `json:"sandbox_id"`
	Reason    string `json:"reason"`
}

func sandboxExpired() *session.Error {
	return &session.Error{Code: "sandbox_expired", Message: "Sandbox retention expired. Create a new session with the required context.", Phase: new("recovery"), Details: []session.Detail{}}
}

// Claim under admission's session lock, without reserving execution capacity.
// The saved operation is also the recovery obligation after an uncertain Delete.
func (e *Executor) claimRetention(ctx context.Context, retryAt time.Time) (*store.Operation, error) {
	var claimed *store.Operation
	now := e.now().Truncate(time.Microsecond)
	cutoff := now.Add(-time.Duration(e.Store.Settings.SandboxPausedRetentionDays) * 24 * time.Hour)
	err := e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, r *store.SessionRecord) error {
		if !r.AllowMultipleRuns || r.SlotReserved || r.SandboxID == nil || (r.SandboxState != "paused" && r.SandboxState != "deleting") {
			return nil
		}
		q := db.New(tx)
		last, err := q.LastSessionRun(ctx, e.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if !last.Status.Terminal() {
			return nil
		}
		active, err := store.ActiveRun(ctx, tx, e.ID)
		if err != nil || active != nil {
			return err
		}
		outstanding, err := openCreate(ctx, tx, e.ID)
		if err != nil || outstanding != nil {
			return err
		}
		var op store.Operation
		if r.SandboxState == "paused" {
			if last.FinishedAt == nil || !last.FinishedAt.Before(cutoff) {
				return nil
			}
			params, err := json.Marshal(retentionDeletion{SandboxID: *r.SandboxID, Reason: "sandbox_expired"})
			if err != nil {
				return err
			}
			op, err = q.CreateOperation(ctx, db.CreateOperationParams{ID: uuid.New(), SessionID: e.ID, Kind: "sandbox_delete", Parameters: params})
			if err != nil {
				return err
			}
		} else {
			op, err = q.SandboxRetentionOperation(ctx, e.ID)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			var params retentionDeletion
			if err := json.Unmarshal(op.Parameters, &params); err != nil {
				return err
			}
			if params.SandboxID != *r.SandboxID || op.Status == "confirmed" || op.NextCheckAt != nil && now.Before(*op.NextCheckAt) {
				return nil
			}
		}
		op.Status = "sending"
		if op.AttemptedAt == nil {
			op.AttemptedAt = &now
		}
		op.NextCheckAt = new(retryAt.UTC().Truncate(time.Microsecond))
		if err := store.SaveOperation(ctx, tx, &op); err != nil {
			return err
		}
		if err := store.UpdateSandbox(ctx, tx, r, func(r *store.SessionRecord) {
			r.SandboxState = "deleting"
			r.SandboxError = sandboxExpired()
		}); err != nil {
			return err
		}
		claimed = &op
		return nil
	})
	return claimed, err
}

// One bounded attempt, never Connect or hooks. Retry scheduling belongs to the
// next cleanup pass, not Executor.Run's reserved-session polling loop.
func (e *Executor) cleanupRetainedSandbox(ctx context.Context, retryAt time.Time) (string, error) {
	op, err := e.claimRetention(ctx, retryAt)
	if err != nil {
		return "failed", err
	}
	if op == nil {
		return "skipped", nil
	}
	var params retentionDeletion
	if err := json.Unmarshal(op.Parameters, &params); err != nil {
		return "failed", err
	}
	callCtx, cancel := context.WithTimeout(ctx, e.Store.Settings.RPCTimeout)
	callErr := e.Platform.Delete(callCtx, params.SandboxID)
	cancel()
	absent := callErr == nil || errors.Is(callErr, harness.ErrNotFound)
	err = e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, r *store.SessionRecord) error {
		current, err := db.New(tx).GetSessionOperation(ctx, db.GetSessionOperationParams{ID: op.ID, SessionID: e.ID})
		if err != nil {
			return err
		}
		// Fence a delayed response from any different attempt or environment.
		if r.SandboxState != "deleting" || r.SandboxID == nil || *r.SandboxID != params.SandboxID || current.Status != "sending" || current.NextCheckAt == nil || !current.NextCheckAt.Equal(*op.NextCheckAt) {
			return errors.New("sandbox retention attempt changed")
		}
		problem := sandboxExpired()
		state := "deleted"
		current.Status, current.NextCheckAt = "confirmed", nil
		if !absent {
			state = "deleting"
			problem = &session.Error{Code: "sandbox_delete_failed", Message: "Sandbox deletion failed; retrying in a later cleanup pass.", Phase: new("recovery"), Details: []session.Detail{}}
			current.Status, current.NextCheckAt = "uncertain", op.NextCheckAt
			if len(current.FirstError) == 0 {
				current.FirstError, err = json.Marshal(problem)
				if err != nil {
					return err
				}
			}
		}
		if err := store.SaveOperation(ctx, tx, &current); err != nil {
			return err
		}
		return store.UpdateSandbox(ctx, tx, r, func(r *store.SessionRecord) {
			r.SandboxState, r.SandboxError = state, problem
			if absent {
				r.SandboxLastKnownState = new("deleted")
			}
		})
	})
	if err != nil {
		return "failed", err
	}
	if !absent {
		return "failed", callErr
	}
	return "deleted", nil
}
