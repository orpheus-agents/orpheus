package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/diagnostic"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
)

// prepareResume covers the first connection of a new run to its existing
// sandbox. A confirmed operation belongs to normal reconnect recovery.
func (e *Executor) prepareResume(ctx context.Context, rid uuid.UUID, sandboxID string) error {
	o, err := e.operation(ctx, "resume", &rid, nil, nil)
	if err != nil {
		return errors.Join(errStoreAccess, err)
	}
	if o.Status == "confirmed" {
		return nil
	}
	if err := e.updateResume(ctx, &o, "", "", nil); err != nil {
		return err
	}
	callCtx, cancel := e.provisionContext(ctx, o)
	state, infoErr := e.Platform.Info(callCtx, sandboxID)
	cancel()
	if err := e.updateResume(ctx, &o, "Info", state, infoErr); err != nil {
		return err
	}
	callCtx, cancel = e.provisionContext(ctx, o)
	box, connectErr := e.Platform.Connect(callCtx, sandboxID, sandboxLeaseTimeout)
	cancel()
	if err := e.updateResume(ctx, &o, "Connect", "", connectErr); err != nil {
		return err
	}
	e.sandbox = box
	e.timeoutRunID, e.timeoutRenewAt = rid, time.Now().Add(time.Minute)
	e.settleAuthSynced, e.settleRetryAt, e.settleRetryDelay = false, time.Time{}, 5*time.Second
	return nil
}

// A session has one executor. The session lock orders each result against API
// cancellation. No RPC is made under that lock or after a terminal decision.
func (e *Executor) updateResume(ctx context.Context, o *store.Operation, method, state string, failure error) error {
	lost := errors.Is(failure, harness.ErrNotFound)
	wait, firstFailure := false, false
	outcome := ""
	var details sandboxFailure
	err := e.Store.Mutate(ctx, e.ID, lost, func(tx pgx.Tx, record *store.SessionRecord) error {
		run, err := store.GetRun(ctx, tx, e.ID, *o.RunID)
		if err != nil {
			return err
		}
		if run.Status.Terminal() {
			wait = true
			return nil
		}
		now := e.now()
		if o.DeadlineAt == nil {
			o.DeadlineAt = new(o.CreatedAt.Add(e.Store.Settings.SandboxProvisionTimeout))
		}
		if failure != nil && o.FirstError == nil {
			details = sandboxFailureDetails(failure)
			details.Method = method
			o.FirstError, err = json.Marshal(details)
			if err != nil {
				return err
			}
			firstFailure = true
		}

		var problem *session.Error
		switch {
		case run.CancelRequestedAt != nil:
			outcome = "cancelled"
		case lost:
			outcome = "sandbox_lost"
			problem = provisionProblem(outcome, "Sandbox is no longer available.")
		case !now.Before(*o.DeadlineAt):
			outcome = "sandbox_resume_timeout"
			problem = provisionProblem(outcome, fmt.Sprintf("Could not resume the sandbox within %s.", o.DeadlineAt.Sub(o.CreatedAt)))
		case errors.Is(failure, harness.ErrRejected):
			outcome = "environment_unavailable"
			problem = provisionProblem(outcome, "Sandbox connection was rejected.")
		}
		if outcome != "" {
			o.Status, o.NextCheckAt = "failed", nil
			if err := store.SaveOperation(ctx, tx, o); err != nil {
				return err
			}
			if lost {
				if err := store.UpdateSandbox(ctx, tx, record, func(r *store.SessionRecord) {
					r.SandboxState, r.SandboxLastKnownState = "unavailable", new("unavailable")
					r.SandboxError = provisionProblem("sandbox_lost", "Sandbox is no longer available.")
					r.SlotReserved = false
				}); err != nil {
					return err
				}
			}
			if err := store.FailUnfinishedHooks(ctx, tx, run.ID, "", nil); err != nil {
				return err
			}
			status := session.Failed
			if run.CancelRequestedAt != nil {
				status = session.Cancelled
			}
			wait = true
			return store.Finish(ctx, tx, record, &run, status, problem, nil)
		}
		switch {
		case failure != nil:
			wait = true
			o.Status = "uncertain"
			o.NextCheckAt = new(now.Add(retryDelay(o.CheckAttempts)))
			if run.Observation == nil || *run.Observation != "reconnecting" {
				run.Observation = new("reconnecting")
				if err := store.PublishRun(ctx, tx, record, &run); err != nil {
					return err
				}
			}
		case method == "":
			if o.NextCheckAt != nil && now.Before(*o.NextCheckAt) {
				wait = true
				return nil
			}
			o.Status = "sending"
			if o.AttemptedAt == nil {
				o.AttemptedAt = &now
			}
			o.CheckAttempts++
			o.NextCheckAt = new(now.Add(retryDelay(o.CheckAttempts)))
		case method == "Info":
			if state == "paused" && record.SandboxState != "resuming" {
				return store.UpdateSandbox(ctx, tx, record, func(r *store.SessionRecord) { r.SandboxState = "resuming" })
			}
			return nil
		case method == "Connect":
			outcome = "ready"
			o.Status, o.NextCheckAt = "confirmed", nil
			if err := store.UpdateSandbox(ctx, tx, record, func(r *store.SessionRecord) {
				r.SandboxState, r.SandboxLastKnownState, r.SandboxError = "ready", new("ready"), nil
			}); err != nil {
				return err
			}
		}
		return store.SaveOperation(ctx, tx, o)
	})
	if err != nil {
		return errors.Join(errStoreAccess, err)
	}
	log := slog.With("session_id", e.ID, "run_id", *o.RunID, "operation_id", o.ID)
	if firstFailure {
		log.WarnContext(ctx, "Sandbox resume request failed", "method", method, "error_kind", details.Code,
			"http_status", details.HTTPStatus, "error_type", diagnostic.Describe(failure))
	}
	if outcome != "" {
		log.InfoContext(ctx, "Sandbox resume finished", "outcome", outcome, "attempts", o.CheckAttempts, "duration", e.now().Sub(o.CreatedAt))
	}
	if wait {
		return errProvisionWait
	}
	return nil
}
