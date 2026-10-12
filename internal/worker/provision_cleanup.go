package worker

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/store"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

// Reuse the reserved-session executor. An empty search never proves that a
// timed-out Create did not allocate a sandbox, even after its lease expired.
func (e *Executor) settleProvision(ctx context.Context, o store.Operation) error {
	adopted := false
	if o.RunID == nil && o.AbandonedAt == nil {
		err := e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, record *store.SessionRecord) error {
			current, err := db.New(tx).GetSessionOperation(ctx, db.GetSessionOperationParams{ID: o.ID, SessionID: e.ID})
			if err != nil {
				return err
			}
			adopted, err = adoptLegacyCreate(ctx, tx, record, &current)
			return err
		})
		if err != nil || adopted {
			return err
		}
	}
	owners, err := db.New(e.Store.Pool).SandboxCreateOwner(ctx, o.ID)
	if err != nil {
		return err
	}
	work, err := db.New(e.Store.Pool).ProvisionHasWork(ctx, e.ID)
	if err != nil {
		return err
	}
	if len(owners) != 1 || (work != nil && *work) {
		return e.state(ctx, "unavailable", provisionProblem("sandbox_provision_conflict", "Sandbox creation ownership is ambiguous; reconciliation requires investigation."))
	}
	if o.AbandonedAt == nil {
		if !owners[0].Status.Terminal() {
			return errProvisionWait
		}
		return e.abandonProvision(ctx, owners[0].ID, o.ID, nil)
	}
	result, err := decodeResult(o)
	if err != nil {
		return err
	}
	if result.SandboxID == "" {
		result, err = e.findProvision(ctx, o)
		if failure, ok := errors.AsType[*harness.ExecutionError](err); ok {
			return e.state(ctx, "unavailable", provisionProblem(failure.Code, failure.Message))
		}
		if err != nil {
			return err
		}
	}
	if result.SandboxID == "" {
		return errProvisionWait
	}
	if err := e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, record *store.SessionRecord) error {
		return store.UpdateSandbox(ctx, tx, record, func(r *store.SessionRecord) {
			r.SandboxID = new(result.SandboxID)
			r.SandboxState = "unavailable"
			r.SandboxError = provisionProblem("sandbox_cleanup_pending", "Abandoned sandbox deletion is pending.")
		})
	}); err != nil {
		return err
	}
	params := struct {
		SandboxID         string    `json:"sandbox_id"`
		CreateOperationID uuid.UUID `json:"create_operation_id"`
	}{result.SandboxID, o.ID}
	deletion, err := e.operation(ctx, "sandbox_delete", &owners[0].ID, nil, params)
	if err != nil {
		return err
	}
	// A different deletion for this run cannot prove this Create was cleaned up.
	var previous struct {
		SandboxID         string    `json:"sandbox_id"`
		CreateOperationID uuid.UUID `json:"create_operation_id"`
	}
	if err := json.Unmarshal(deletion.Parameters, &previous); err != nil || previous.SandboxID != params.SandboxID || previous.CreateOperationID != params.CreateOperationID {
		return e.state(ctx, "unavailable", provisionProblem("sandbox_provision_conflict", "Sandbox deletion does not match the abandoned creation."))
	}
	if deletion.Status != "confirmed" {
		due, err := e.reserveCheck(ctx, &deletion)
		if err != nil {
			return err
		}
		if !due {
			return errProvisionWait
		}
		if err := e.opStatus(ctx, deletion.ID, "sending", nil); err != nil {
			return err
		}
		e.Disconnect()
		callCtx, cancel := e.provisionContext(ctx, deletion)
		callErr := e.Platform.Delete(callCtx, result.SandboxID)
		cancel()
		if callErr != nil && !errors.Is(callErr, harness.ErrNotFound) {
			if err := e.saveProvisionResult(ctx, deletion, "uncertain", nil, callErr); err != nil {
				return err
			}
			return errProvisionWait
		}
	}
	return e.Store.Mutate(ctx, e.ID, true, func(tx pgx.Tx, record *store.SessionRecord) error {
		current, err := db.New(tx).GetSessionOperation(ctx, db.GetSessionOperationParams{ID: deletion.ID, SessionID: e.ID})
		if err != nil {
			return err
		}
		current.Status, current.NextCheckAt = "confirmed", nil
		if err := store.SaveOperation(ctx, tx, &current); err != nil {
			return err
		}
		outstanding, err := openCreate(ctx, tx, e.ID)
		if err != nil {
			return err
		}
		active, err := store.ActiveRun(ctx, tx, e.ID)
		if err != nil {
			return err
		}
		if outstanding == nil && active == nil {
			record.SlotReserved = false
			return store.UpdateSandbox(ctx, tx, record, func(r *store.SessionRecord) { r.SandboxError = nil })
		}
		return nil
	})
}
