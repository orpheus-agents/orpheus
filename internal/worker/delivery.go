package worker

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
)

func (e *Executor) send(ctx context.Context, record store.SessionRecord, run store.RunRecord) error {
	messages, err := store.Messages(ctx, e.Store.Pool, run.ID)
	if err != nil {
		return err
	}
	index := slices.IndexFunc(messages, func(m store.MessageRecord) bool {
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
			if candidate.Role == "user" && candidate.DeliveryNumber != nil {
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
	if o.Status == "sending" || o.Status == "uncertain" {
		return e.observation(ctx, "uncertain")
	}
	if o.Status == "confirmed" {
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
		send = true
		return nil
	})
	if err != nil || !send {
		return err
	}
	result, deliveryErr := e.invoke(ctx, o, func() (operationResult, error) {
		if kind == "start" {
			texts := make([]string, len(batch))
			for i, input := range batch {
				texts[i] = input.Text
			}
			id, err := e.driver.Start(ctx, record.Configuration.Public.Agent, *record.ThreadID, texts)
			return operationResult{TurnID: id}, err
		}
		return operationResult{}, e.driver.Steer(ctx, *record.ThreadID, *run.NativeTurnID, message.Text)
	})
	if deliveryErr != nil {
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
			return err
		}
		if confirmed {
			if kind == "start" {
				return harness.Failure(problem.Code, problem.Message)
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
	if o.Status != "pending" || e.driver == nil || record.ThreadID == nil || run.NativeTurnID == nil {
		return nil
	}
	if err := e.opStatus(ctx, o.ID, "sending", nil); err != nil {
		return err
	}
	err = e.driver.Interrupt(ctx, *record.ThreadID, *run.NativeTurnID)
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
	if run.ExecutionStartedAt != nil && run.NativeTurnID == nil {
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
