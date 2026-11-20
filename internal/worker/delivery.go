package worker

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/diagnostic"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

func (e *Executor) send(ctx context.Context, record store.SessionRecord, run store.RunRecord) error {
	messages, err := store.Messages(ctx, e.Store.Pool, run.ID)
	if err != nil {
		return err
	}
	nativeIDs := record.Configuration.Harness == "opencode"
	accepted := map[uuid.UUID]bool{}
	var initialEnd *int
	if nativeIDs {
		journal, err := db.New(e.Store.Pool).ReconcileOperations(ctx, db.ReconcileOperationsParams{SessionID: e.ID, RunIds: []uuid.UUID{run.ID}})
		if err != nil {
			return err
		}
		for _, op := range journal {
			if op.MessageID == nil {
				continue
			}
			if op.Kind == "start" {
				for _, m := range messages {
					if m.ID == *op.MessageID {
						initialEnd = m.DeliveryNumber
						break
					}
				}
			} else if op.Status == "confirmed" {
				accepted[*op.MessageID] = true
			}
		}
	}
	index := slices.IndexFunc(messages, func(m store.MessageRecord) bool {
		if nativeIDs && run.NativeTurnID != nil && (accepted[m.ID] || (initialEnd != nil && m.DeliveryNumber != nil && *m.DeliveryNumber <= *initialEnd)) {
			return false
		}
		return m.Role == "user" && m.DeliveryStatus != nil && slices.Contains([]string{"pending", "sending", "uncertain"}, *m.DeliveryStatus)
	})
	if index < 0 {
		return nil
	}
	message := messages[index]
	kind := "steer"
	batch := []store.MessageRecord{message}
	if run.NativeTurnID == nil {
		kind = "start"
		batch = nil
		for _, candidate := range messages {
			if candidate.Role == "user" && candidate.DeliveryNumber != nil && (initialEnd == nil || *candidate.DeliveryNumber <= *initialEnd) {
				batch = append(batch, candidate)
			}
		}
		message = batch[len(batch)-1]
	}
	params := struct {
		ThreadID      *string  `json:"thread_id"`
		TurnID        *string  `json:"turn_id"`
		PreviousTurns []string `json:"previous_turns"`
		PreviousItems []string `json:"previous_items"`
	}{ThreadID: record.ThreadID, TurnID: run.NativeTurnID, PreviousTurns: []string{}, PreviousItems: []string{}}
	for _, turn := range e.snapshot.Turns {
		params.PreviousTurns = append(params.PreviousTurns, turn.NativeID)
		if run.NativeTurnID != nil && turn.NativeID == *run.NativeTurnID {
			for _, item := range turn.Items {
				params.PreviousItems = append(params.PreviousItems, item.NativeID)
			}
		}
	}
	o, err := e.operation(ctx, kind, &run.ID, &message.ID, params)
	if err != nil {
		return err
	}
	replay := o.Status == "sending" || o.Status == "uncertain"
	if replay && !nativeIDs {
		return e.observation(ctx, "uncertain")
	}
	if o.Status == "confirmed" && !nativeIDs {
		return nil
	}
	send := false
	// Persist the original deadline and delivery attempt before external I/O.
	// A concurrent cancellation observed here prevents any external dispatch.
	err = e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, r *store.SessionRecord) error {
		current, err := store.GetRun(ctx, tx, e.ID, run.ID)
		if err != nil {
			return err
		}
		if current.CancelRequestedAt != nil || current.Status.Terminal() {
			return nil
		}
		if store.BudgetExhausted(*r) {
			return store.EnforceTokenBudget(ctx, tx, r, &current)
		}
		for _, input := range batch {
			m, err := store.GetMessage(ctx, tx, input.ID)
			if err != nil {
				return err
			}
			m.DeliveryStatus = new("sending")
			if err := store.PublishMessage(ctx, tx, r, &m); err != nil {
				return err
			}
		}
		if kind == "start" && current.ExecutionStartedAt == nil {
			now := time.Now().UTC()
			current.ExecutionStartedAt = &now
			current.DeadlineAt = new(now.Add(time.Duration(record.Configuration.Public.Limits.RunTimeoutSeconds) * time.Second))
			current.Phase = new("agent")
			if err := store.PublishRun(ctx, tx, r, &current); err != nil {
				return err
			}
		}
		run = current
		send = true
		return nil
	})
	if err != nil || !send {
		return err
	}
	delivery := harness.Delivery{RunID: run.ID.String(), OperationID: o.ID.String(), Replay: replay}
	for _, m := range batch {
		delivery.Inputs = append(delivery.Inputs, harness.Input{ID: m.ID.String(), Text: m.Text})
	}
	dispatch := func() (operationResult, error) {
		id, err := e.driver.Deliver(ctx, record.Configuration.Public.Agent, *record.ThreadID, run.NativeTurnID, delivery)
		return operationResult{TurnID: id}, err
	}
	var result operationResult
	var deliveryErr error
	if replay {
		result, deliveryErr = dispatch()
		if deliveryErr == nil {
			deliveryErr = e.opStatus(ctx, o.ID, "confirmed", result)
		}
	} else {
		result, deliveryErr = e.invoke(ctx, o, dispatch)
	}
	if deliveryErr != nil {
		deliveryErr = e.bindFailure(deliveryErr, record, &run)
		fields := diagnostic.Capture(deliveryErr)
		problem := (*session.Error)(nil)
		status := "uncertain"
		confirmed := errors.Is(deliveryErr, harness.ErrRejected)
		if failure, ok := errors.AsType[*harness.ExecutionError](deliveryErr); ok {
			problem = &session.Error{Code: failure.Code, Message: failure.Message, Details: []session.Detail{}}
			confirmed = true
		}
		if confirmed {
			status = "rejected"
			if problem == nil {
				problem = &session.Error{Code: "run_finished_before_delivery", Message: "Harness no longer accepts this message.", Details: []session.Detail{}}
				if kind == "start" {
					problem.Code = "harness_failed"
					problem.Message = "Harness rejected the assignment."
				}
			}
			problem.DiagnosticID = &fields.ID
		}
		if err := e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, r *store.SessionRecord) error {
			for _, input := range batch {
				m, err := store.GetMessage(ctx, tx, input.ID)
				if err != nil {
					return err
				}
				m.DeliveryStatus = &status
				m.Error = problem
				if err := store.PublishMessage(ctx, tx, r, &m); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return diagnostic.With(err, fields)
		}
		if confirmed {
			if kind == "start" {
				return harness.FailureCause(problem.Code, problem.Message, deliveryErr)
			}
			fields.PublicCode = problem.Code
			if ctx.Err() == nil {
				diagnostic.Log(ctx, slog.LevelError, fields)
			}
			return nil
		}
		return deliveryErr
	}
	return e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, r *store.SessionRecord) error {
		current, err := store.GetRun(ctx, tx, e.ID, run.ID)
		if err != nil {
			return err
		}
		for _, input := range batch {
			m, err := store.GetMessage(ctx, tx, input.ID)
			if err != nil {
				return err
			}
			if nativeIDs {
				// Admission to the OpenCode inbox precedes projection into model history.
				// Reconcile confirms the persisted input ID on the next observation.
				continue
			}
			m.DeliveryStatus = new("delivered")
			if err := store.PublishMessage(ctx, tx, r, &m); err != nil {
				return err
			}
		}
		if kind == "start" {
			current.NativeTurnID = &result.TurnID
			if current.CancelRequestedAt == nil {
				current.Status = session.Running
			}
			current.Observation = new("attached")
			return store.PublishRun(ctx, tx, r, &current)
		}
		return nil
	})
}
func (e *Executor) cancelRun(ctx context.Context, record store.SessionRecord, run store.RunRecord) error {
	if run.ExecutionStartedAt == nil {
		return e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, r *store.SessionRecord) error {
			current, err := store.GetRun(ctx, tx, e.ID, run.ID)
			if err != nil {
				return err
			}
			return store.Finish(ctx, tx, r, &current, session.Cancelled, nil, nil)
		})
	}
	o, err := e.operation(ctx, "cancel", &run.ID, nil, nil)
	if err != nil {
		return err
	}
	if run.CancelAttemptedAt == nil {
		if err := e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, r *store.SessionRecord) error {
			current, err := store.GetRun(ctx, tx, e.ID, run.ID)
			if err != nil {
				return err
			}
			current.CancelAttemptedAt = new(time.Now().UTC())
			return store.PublishRun(ctx, tx, r, &current)
		}); err != nil {
			return err
		}
	}
	turn := run.NativeTurnID
	if turn == nil && record.Configuration.Harness == "opencode" {
		turn = new(run.ID.String())
	}
	if o.Status != "pending" || e.driver == nil || record.ThreadID == nil || turn == nil {
		return nil
	}
	if err := e.opStatus(ctx, o.ID, "sending", nil); err != nil {
		return err
	}
	err = e.driver.Interrupt(ctx, *record.ThreadID, *turn)
	if errors.Is(err, harness.ErrRejected) {
		return e.opStatus(ctx, o.ID, "confirmed", operationResult{Accepted: new(false)})
	}
	if err != nil {
		if saveErr := e.opStatus(ctx, o.ID, "uncertain", nil); saveErr != nil {
			return saveErr
		}
		return err
	}
	return e.opStatus(ctx, o.ID, "confirmed", nil)
}
func (e *Executor) forceStop(ctx context.Context, record store.SessionRecord, run store.RunRecord) error {
	processes, err := e.sandbox.Processes(ctx)
	if err != nil {
		return err
	}
	process := matching(processes, record.ProcessID, record.LaunchID)
	if process != nil {
		o, err := e.operation(ctx, "kill", &run.ID, nil, nil)
		if err != nil {
			return err
		}
		if err := e.opStatus(ctx, o.ID, "sending", nil); err != nil {
			return err
		}
		if err := e.sandbox.Kill(ctx, process.PID); err != nil {
			return err
		}
		if err := e.opStatus(ctx, o.ID, "confirmed", nil); err != nil {
			return err
		}
	}
	if e.driver != nil {
		if e.unregisterLimits != nil {
			e.unregisterLimits()
			e.unregisterLimits = nil
		}
		_ = e.driver.Close()
		e.driver = nil
	}
	if record.Configuration.Harness != "opencode" && run.ExecutionStartedAt != nil && run.NativeTurnID == nil {
		messages, err := store.Messages(ctx, e.Store.Pool, run.ID)
		if err != nil {
			return err
		}
		initialInputs := 0
		for _, message := range messages {
			if message.Role == "user" && message.DeliveryNumber != nil {
				initialInputs++
			}
		}
		if initialInputs > 1 {
			return harness.Failure("context_lost", "The batched start outcome could not be confirmed before cancellation.")
		}
	}
	return nil
}
