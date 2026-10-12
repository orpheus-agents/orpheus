package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/diagnostic"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

// Waiting and terminal preparation both stop this tick before any user work.
var errProvisionWait = errors.New("sandbox provisioning cannot continue this tick")

var (
	errProvisionNotFound = errors.New("sandbox not found yet")
	errProvisionMultiple = errors.New("multiple sandbox matches")
)

func (e *Executor) now() time.Time {
	if e.clock != nil {
		return e.clock().UTC()
	}
	return time.Now().UTC()
}

func openCreate(ctx context.Context, q db.DBTX, sid uuid.UUID) (*store.Operation, error) {
	o, err := db.New(q).OpenSandboxCreate(ctx, sid)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &o, err
}

func provisionProblem(code, message string) *session.Error {
	return &session.Error{Code: code, Message: message, Phase: new("preparation"), Details: []session.Detail{}}
}

// Recheck intent under the same session lock as cancellation. Mutations of the
// journal and adoption never rely on the executor's copy from before network I/O.
func (e *Executor) provision(ctx context.Context, rid uuid.UUID, create bool, change func(pgx.Tx, *store.SessionRecord, *store.Operation) error) (*store.Operation, error) {
	// Adopted environments use the normal lifecycle. Avoid the global capacity
	// lock on every running tick; every provisioning mutation still rechecks below.
	if change == nil {
		o, err := openCreate(ctx, e.Store.Pool, e.ID)
		if err != nil {
			return nil, errors.Join(errStoreAccess, err)
		}
		if o == nil {
			record, err := store.GetSession(ctx, e.Store.Pool, e.ID, false)
			if err != nil {
				return nil, errors.Join(errStoreAccess, err)
			}
			if record.SandboxID != nil {
				return nil, nil
			}
		}
	}
	var out *store.Operation
	stopped := false
	err := e.Store.Mutate(ctx, e.ID, true, func(tx pgx.Tx, record *store.SessionRecord) error {
		run, err := store.GetRun(ctx, tx, e.ID, rid)
		if err != nil {
			return err
		}
		if run.Status.Terminal() {
			stopped = true
			return nil
		}
		o, err := openCreate(ctx, tx, e.ID)
		if err != nil {
			return err
		}
		if o == nil {
			found, err := db.New(tx).CurrentSandboxCreate(ctx, db.CurrentSandboxCreateParams{SessionID: e.ID, ID: rid})
			if err == nil {
				o = &found
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if o != nil {
			result, err := decodeResult(*o)
			if err != nil {
				return err
			}
			if result.Adopted {
				return nil
			}
			adopted, err := adoptLegacyCreate(ctx, tx, record, o)
			if err != nil {
				return err
			}
			if adopted {
				return nil
			}
			if o.DeadlineAt == nil {
				began := o.CreatedAt
				if o.AttemptedAt != nil {
					began = *o.AttemptedAt
				}
				o.DeadlineAt = new(began.Add(120 * time.Second))
				if err := store.SaveOperation(ctx, tx, o); err != nil {
					return err
				}
			}
		} else if record.SandboxID != nil {
			return nil
		}
		// Legacy sessions that already launched a hook/harness use normal stopping.
		work, err := db.New(tx).ProvisionHasWork(ctx, e.ID)
		if err != nil {
			return err
		}
		if (work != nil && *work) || run.ExecutionStartedAt != nil {
			return nil
		}
		if o == nil && create && run.CancelRequestedAt == nil {
			created, err := db.New(tx).CreateOperation(ctx, db.CreateOperationParams{ID: uuid.New(), SessionID: e.ID, RunID: &rid, Kind: "sandbox_create", Parameters: json.RawMessage(`{}`)})
			if err != nil {
				return err
			}
			o = &created
			o.DeadlineAt = new(e.now().Add(e.Store.Settings.SandboxProvisionTimeout))
			if err := store.SaveOperation(ctx, tx, o); err != nil {
				return err
			}
		}
		var problem *session.Error
		if o != nil {
			owners, err := db.New(tx).SandboxCreateOwner(ctx, o.ID)
			if err != nil {
				return err
			}
			if len(owners) != 1 || owners[0].ID != rid {
				// No new Create or destructive recovery based on an ambiguous old record.
				stopped = true
				return e.finishProvision(ctx, tx, record, &run, o, provisionProblem("sandbox_provision_conflict", "Sandbox creation cannot be associated with this run."))
			}
			if o.Status == "failed" {
				problem = provisionProblem("environment_unavailable", "Sandbox creation was rejected.")
			}
			if o.DeadlineAt != nil && !e.now().Before(*o.DeadlineAt) {
				problem = provisionProblem("sandbox_provision_timeout", "Sandbox provisioning timed out.")
			}
			if o.AbandonedAt != nil {
				problem = provisionProblem("sandbox_provision_conflict", "Sandbox creation has been abandoned.")
			}
		}
		if run.CancelRequestedAt != nil || problem != nil {
			stopped = true
			return e.finishProvision(ctx, tx, record, &run, o, problem)
		}
		if o != nil && change != nil {
			if err := change(tx, record, o); err != nil {
				return err
			}
		}
		out = o
		return nil
	})
	if err != nil {
		return nil, errors.Join(errStoreAccess, err)
	}
	if stopped {
		return nil, errProvisionWait
	}
	return out, nil
}

func (e *Executor) finishProvision(ctx context.Context, tx pgx.Tx, record *store.SessionRecord, run *store.RunRecord, o *store.Operation, problem *session.Error) error {
	outstanding := o != nil && o.Status != "pending" && o.Status != "failed"
	if o != nil {
		if outstanding {
			o.AbandonedAt = new(e.now())
		} else {
			o.Status = "failed"
		}
		if err := store.SaveOperation(ctx, tx, o); err != nil {
			return err
		}
	}
	if err := store.UpdateSandbox(ctx, tx, record, func(r *store.SessionRecord) {
		if outstanding {
			r.SandboxState = "unavailable"
			r.SandboxLastKnownState = new("unavailable")
			r.SandboxError = provisionProblem("sandbox_cleanup_pending", "Sandbox creation was abandoned; resource reconciliation is pending.")
			r.SlotReserved = true
		} else {
			r.SandboxState = "not_created"
			r.SandboxError = nil
			r.SlotReserved = false
		}
	}); err != nil {
		return err
	}
	if run.Status.Terminal() {
		return nil
	}
	if err := store.FailUnfinishedHooks(ctx, tx, run.ID, "", nil); err != nil {
		return err
	}
	status := session.Failed
	if run.CancelRequestedAt != nil {
		status, problem = session.Cancelled, nil
	}
	return store.Finish(ctx, tx, record, run, status, problem, nil)
}

func (e *Executor) provisionContext(ctx context.Context, o store.Operation) (context.Context, context.CancelFunc) {
	timeout := e.Store.Settings.RPCTimeout
	if o.DeadlineAt != nil && o.AbandonedAt == nil {
		timeout = min(timeout, o.DeadlineAt.Sub(e.now()))
	}
	return context.WithTimeout(ctx, timeout)
}

func retryDelay(attempt int) time.Duration {
	d := min(60*time.Second, 5*time.Second<<min(max(attempt-1, 0), 4))
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

func (e *Executor) reserveCheck(ctx context.Context, o *store.Operation) (bool, error) {
	due := false
	err := e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, _ *store.SessionRecord) error {
		current, err := db.New(tx).GetSessionOperation(ctx, db.GetSessionOperationParams{ID: o.ID, SessionID: e.ID})
		if err != nil {
			return err
		}
		if current.NextCheckAt != nil && e.now().Before(*current.NextCheckAt) {
			return nil
		}
		current.CheckAttempts = min(current.CheckAttempts+1, 1000000)
		current.NextCheckAt = new(e.now().Add(retryDelay(current.CheckAttempts)))
		if err := store.SaveOperation(ctx, tx, &current); err != nil {
			return err
		}
		*o, due = current, true
		return nil
	})
	return due, err
}

func (e *Executor) saveProvisionResult(ctx context.Context, o store.Operation, status string, result *operationResult, failure error) error {
	return e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, _ *store.SessionRecord) error {
		current, err := db.New(tx).GetSessionOperation(ctx, db.GetSessionOperationParams{ID: o.ID, SessionID: e.ID})
		if err != nil {
			return err
		}
		current.Status = status
		if result != nil {
			current.Result, err = json.Marshal(result)
			if err != nil {
				return err
			}
		}
		if failure != nil {
			if current.FirstError == nil {
				code := "platform_error"
				if errors.Is(failure, context.DeadlineExceeded) {
					code = "deadline_exceeded"
				}
				if errors.Is(failure, harness.ErrRejected) {
					code = "rejected"
				}
				if errors.Is(failure, harness.ErrNotFound) {
					code = "not_found"
				}
				if errors.Is(failure, errProvisionNotFound) {
					code = "sandbox_not_found_yet"
				}
				if errors.Is(failure, errProvisionMultiple) {
					code = "multiple_matches"
				}
				details := map[string]any{"code": code}
				if upstream, ok := errors.AsType[interface {
					error
					HTTPStatus() int
				}](failure); ok {
					details["http_status"] = upstream.HTTPStatus()
				}
				current.FirstError, err = json.Marshal(details)
				if err != nil {
					return err
				}
				level := slog.LevelWarn
				if errors.Is(failure, errProvisionNotFound) {
					level = slog.LevelDebug
				}
				slog.Log(ctx, level, "Sandbox operation unresolved", "session_id", e.ID, "operation_id", o.ID, "code", code, "error_type", diagnostic.Describe(failure))
			} else {
				slog.DebugContext(ctx, "Sandbox operation still unresolved", "session_id", e.ID, "operation_id", o.ID, "error_type", diagnostic.Describe(failure))
			}
			current.NextCheckAt = new(e.now().Add(retryDelay(max(1, current.CheckAttempts))))
		} else {
			current.NextCheckAt = nil
		}
		return store.SaveOperation(ctx, tx, &current)
	})
}

func (e *Executor) findProvision(ctx context.Context, o store.Operation) (operationResult, error) {
	due, err := e.reserveCheck(ctx, &o)
	if err != nil {
		return operationResult{}, err
	}
	if !due {
		return operationResult{}, errProvisionWait
	}
	callCtx, cancel := e.provisionContext(ctx, o)
	found, findErr := e.Platform.Find(callCtx, map[string]string{"orpheus_session_id": e.ID.String(), "orpheus_operation_id": o.ID.String()})
	cancel()
	if findErr == nil && len(found) == 1 {
		result := operationResult{SandboxID: found[0]}
		return result, e.saveProvisionResult(ctx, o, "confirmed", &result, nil)
	}
	if findErr == nil {
		findErr = errProvisionNotFound
		if len(found) > 1 {
			findErr = errProvisionMultiple
		}
	}
	if err := e.saveProvisionResult(ctx, o, o.Status, nil, findErr); err != nil {
		return operationResult{}, err
	}
	if len(found) > 1 {
		return operationResult{}, harness.Failure("sandbox_provision_conflict", "Multiple sandboxes match the creation operation.")
	}
	return operationResult{}, errProvisionWait
}

func (e *Executor) ensureProvision(ctx context.Context, record *store.SessionRecord, run *store.RunRecord, o store.Operation) error {
	result, err := decodeResult(o)
	if err != nil {
		return err
	}
	if o.Status == "pending" {
		_, err = e.provision(ctx, run.ID, false, func(tx pgx.Tx, r *store.SessionRecord, current *store.Operation) error {
			current.Status, current.AttemptedAt = "sending", new(e.now())
			if err := store.SaveOperation(ctx, tx, current); err != nil {
				return err
			}
			return store.UpdateSandbox(ctx, tx, r, func(r *store.SessionRecord) { r.SandboxState = "provisioning" })
		})
		if err != nil {
			return err
		}
		callCtx, cancel := e.provisionContext(ctx, o)
		box, callErr := e.Platform.Create(callCtx, record.Configuration.Public.Sandbox.Template, sandboxLeaseTimeout, map[string]string{"orpheus_session_id": e.ID.String(), "orpheus_operation_id": o.ID.String()})
		cancel()
		status := "confirmed"
		if callErr != nil {
			status = "uncertain"
			if errors.Is(callErr, harness.ErrRejected) || errors.Is(callErr, harness.ErrNotFound) {
				status = "failed"
			}
		} else {
			result.SandboxID = box.ID()
		}
		if err := e.saveProvisionResult(ctx, o, status, &result, callErr); err != nil {
			return err
		}
		if _, err := e.provision(ctx, run.ID, false, nil); err != nil {
			return err
		}
		if callErr != nil {
			if err := e.observation(ctx, "uncertain"); err != nil {
				return err
			}
			return errProvisionWait
		}
		e.sandbox = box
	} else if result.SandboxID == "" {
		result, err = e.findProvision(ctx, o)
		if _, guardErr := e.provision(ctx, run.ID, false, nil); guardErr != nil {
			return guardErr
		}
		if failure, ok := errors.AsType[*harness.ExecutionError](err); ok {
			return e.abandonProvision(ctx, run.ID, o.ID, provisionProblem(failure.Code, failure.Message))
		}
		if err != nil {
			return err
		}
	}
	if result.SandboxID == "" {
		return errProvisionWait
	}
	if e.sandbox == nil {
		due, err := e.reserveCheck(ctx, &o)
		if err != nil {
			return err
		}
		if !due {
			return errProvisionWait
		}
		callCtx, cancel := e.provisionContext(ctx, o)
		_, callErr := e.Platform.Info(callCtx, result.SandboxID)
		cancel()
		if _, err := e.provision(ctx, run.ID, false, nil); err != nil {
			return err
		}
		if callErr != nil {
			return e.provisionConnectError(ctx, o, callErr)
		}
		callCtx, cancel = e.provisionContext(ctx, o)
		box, callErr := e.Platform.Connect(callCtx, result.SandboxID, sandboxLeaseTimeout)
		cancel()
		if _, err := e.provision(ctx, run.ID, false, nil); err != nil {
			return err
		}
		if callErr != nil {
			return e.provisionConnectError(ctx, o, callErr)
		}
		e.sandbox = box
	}
	_, err = e.provision(ctx, run.ID, false, func(tx pgx.Tx, r *store.SessionRecord, current *store.Operation) error {
		result.Adopted = true
		current.Result, _ = json.Marshal(result)
		current.NextCheckAt = nil
		if err := store.SaveOperation(ctx, tx, current); err != nil {
			return err
		}
		return store.UpdateSandbox(ctx, tx, r, func(r *store.SessionRecord) {
			r.SandboxID = new(result.SandboxID)
			r.SandboxState, r.SandboxLastKnownState, r.SandboxError = "ready", new("ready"), nil
		})
	})
	if err != nil {
		e.sandbox = nil
		return err
	}
	record.SandboxID = new(result.SandboxID)
	record.SandboxState = "ready"
	e.timeoutRunID, e.timeoutRenewAt = run.ID, time.Now().Add(time.Minute)
	e.settleAuthSynced, e.settleRetryAt, e.settleRetryDelay = false, time.Time{}, 5*time.Second
	return nil
}

func (e *Executor) provisionConnectError(ctx context.Context, o store.Operation, err error) error {
	if saveErr := e.saveProvisionResult(ctx, o, "confirmed", nil, err); saveErr != nil {
		return saveErr
	}
	return errProvisionWait
}

func (e *Executor) abandonProvision(ctx context.Context, rid, oid uuid.UUID, problem *session.Error) error {
	err := e.Store.Mutate(ctx, e.ID, true, func(tx pgx.Tx, record *store.SessionRecord) error {
		run, err := store.GetRun(ctx, tx, e.ID, rid)
		if err != nil {
			return err
		}
		o, err := db.New(tx).GetSessionOperation(ctx, db.GetSessionOperationParams{ID: oid, SessionID: e.ID})
		if err != nil {
			return err
		}
		return e.finishProvision(ctx, tx, record, &run, &o, problem)
	})
	if err != nil {
		return err
	}
	return errProvisionWait
}

// The old worker can confirm/adopt a legacy Create after the data migration.
// New attempts have a run ID and can only be adopted by the guarded transition.
func adoptLegacyCreate(ctx context.Context, tx pgx.Tx, record *store.SessionRecord, o *store.Operation) (bool, error) {
	if o.RunID != nil || o.AbandonedAt != nil || o.Status != "confirmed" || record.SandboxID == nil {
		return false, nil
	}
	result, err := decodeResult(*o)
	if err != nil {
		return false, err
	}
	used := record.Workspace != nil || record.LaunchID != nil
	if record.SandboxLastKnownState != nil && (*record.SandboxLastKnownState == "ready" || *record.SandboxLastKnownState == "paused") {
		used = true
	}
	switch record.SandboxState {
	case "ready", "paused", "pausing", "deleting", "deleted":
		used = true
	}
	if !used || result.SandboxID != *record.SandboxID {
		return false, nil
	}
	result.Adopted = true
	o.Result, err = json.Marshal(result)
	if err != nil {
		return false, err
	}
	return true, store.SaveOperation(ctx, tx, o)
}
