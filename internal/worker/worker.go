// Package worker executes sessions concurrently under a single database owner.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skillum-ai/orpheus/internal/credentials"
	"github.com/skillum-ai/orpheus/internal/diagnostic"
	"github.com/skillum-ai/orpheus/internal/harness"
	"github.com/skillum-ai/orpheus/internal/harness/codex"
	"github.com/skillum-ai/orpheus/internal/session"
	"github.com/skillum-ai/orpheus/internal/store"
)

type DriverFactory func(harness.Sandbox) harness.Driver
type AccountFactory func(context.Context, harness.Sandbox, string, session.Credentials) (credentials.Sync, error)
type Executor struct {
	ID              uuid.UUID
	Store           *store.Store
	Platform        harness.Platform
	NewDriver       DriverFactory
	NewAccount      AccountFactory
	RunnerBinary    func(string) ([]byte, error)
	sandbox         harness.Sandbox
	driver          harness.Driver
	account         credentials.Sync
	snapshot        harness.Snapshot
	pauseRetryAt    time.Time
	pauseRetryDelay time.Duration
	pauseAuthSynced bool
	timeoutRenewAt  time.Time
	timeoutRunID    uuid.UUID
	runnerReady     bool
}

func NewExecutor(id uuid.UUID, s *store.Store, p harness.Platform) *Executor {
	return &Executor{ID: id, Store: s, Platform: p, NewDriver: func(box harness.Sandbox) harness.Driver {
		return codex.New(box, s.Settings.RPCTimeout, s.Settings.MaxToolResultBytes)
	}, RunnerBinary: os.ReadFile, NewAccount: func(ctx context.Context, box harness.Sandbox, home string, source session.Credentials) (credentials.Sync, error) {
		return credentials.New(ctx, box, home, source)
	}, pauseRetryDelay: 5 * time.Second}
}
func (e *Executor) read(ctx context.Context) (store.SessionRecord, *store.RunRecord, error) {
	return e.Store.Read(ctx, e.ID)
}
func (e *Executor) state(ctx context.Context, state string, problem *session.Error) error {
	return e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, r *store.SessionRecord) error {
		if r.SandboxState == "unavailable" && state != "unavailable" {
			return nil
		}
		return store.UpdateSandbox(ctx, tx, r, func(r *store.SessionRecord) {
			r.SandboxState = state
			if state == "ready" || state == "paused" || state == "unavailable" {
				r.SandboxLastKnownState = &state
			}
			r.SandboxError = problem
		})
	})
}
func (e *Executor) observation(ctx context.Context, value string) error {
	return e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, r *store.SessionRecord) error {
		run, err := store.ActiveRun(ctx, tx, e.ID)
		if err != nil {
			return err
		}
		if run != nil && (run.Observation == nil || *run.Observation != value) {
			run.Observation = &value
			return store.PublishRun(ctx, tx, r, run)
		}
		return nil
	})
}

type operationResult struct {
	SandboxID   string  `json:"sandbox_id,omitzero"`
	PID         int     `json:"pid,omitzero"`
	TurnID      string  `json:"turn_id,omitzero"`
	ID          string  `json:"id,omitzero"`
	Path        *string `json:"path,omitzero"`
	Initialized bool    `json:"initialized,omitzero"`
	Accepted    *bool   `json:"accepted,omitzero"`
}

func decodeResult(o store.Operation) (operationResult, error) {
	var out operationResult
	if len(o.Result) == 0 {
		return out, nil
	}
	err := json.Unmarshal(o.Result, &out)
	return out, err
}
func (e *Executor) operation(ctx context.Context, kind string, rid, mid *uuid.UUID, params any) (store.Operation, error) {
	return e.Store.Operation(ctx, e.ID, kind, rid, mid, params)
}
func (e *Executor) opStatus(ctx context.Context, id uuid.UUID, status string, result any) error {
	return e.Store.SetOperation(ctx, e.ID, id, status, result)
}
func (e *Executor) invoke(ctx context.Context, o store.Operation, call func() (operationResult, error)) (operationResult, error) {
	if o.Status == "confirmed" {
		return decodeResult(o)
	}
	if o.Status != "pending" {
		return operationResult{}, harness.ErrUncertain
	}
	if err := e.opStatus(ctx, o.ID, "sending", nil); err != nil {
		return operationResult{}, err
	}
	result, err := call()
	if err != nil {
		if errors.Is(err, harness.ErrRejected) || errors.Is(err, harness.ErrNotFound) {
			if saveErr := e.opStatus(ctx, o.ID, "failed", nil); saveErr != nil {
				return result, saveErr
			}
			if errors.Is(err, harness.ErrEnvironmentRejected) {
				return result, harness.Failure("environment_unavailable", "Sandbox operation was rejected.")
			}
			return result, err
		}
		if saveErr := e.opStatus(ctx, o.ID, "uncertain", nil); saveErr != nil {
			return result, saveErr
		}
		return result, harness.ErrUncertain
	}
	if err := e.opStatus(ctx, o.ID, "confirmed", result); err != nil {
		return result, err
	}
	return result, nil
}
func (e *Executor) failure(ctx context.Context, f *harness.ExecutionError) error {
	unavailable := f.Code == "sandbox_lost" || f.Code == "context_lost"
	return e.Store.Mutate(ctx, e.ID, unavailable, func(tx pgx.Tx, r *store.SessionRecord) error {
		// Admission must see termination and loss of the environment atomically.
		// Otherwise a new run can enter between Finish and the unavailable state.
		if unavailable {
			if err := store.UpdateSandbox(ctx, tx, r, func(r *store.SessionRecord) {
				r.SandboxState = "unavailable"
				r.SandboxLastKnownState = new("unavailable")
				r.SandboxError = &session.Error{Code: f.Code, Message: f.Message, Phase: new("recovery"), Details: []session.Detail{}}
				if f.Code == "sandbox_lost" {
					r.SlotReserved = false
				}
			}); err != nil {
				return err
			}
		}
		run, err := store.ActiveRun(ctx, tx, e.ID)
		if err != nil {
			return err
		}
		if run == nil {
			return nil
		}
		phase := "preparation"
		if run.ExecutionStartedAt != nil {
			phase = "execution"
		}
		if run.Phase != nil && *run.Phase == "after_run" {
			phase = "finalization"
		}
		problem := &session.Error{Code: f.Code, Message: f.Message, Phase: &phase, Details: []session.Detail{}}
		activeHook := ""
		if run.Phase != nil && (*run.Phase == "after_create" || *run.Phase == "before_run" || *run.Phase == "after_run") {
			activeHook = *run.Phase
		}
		if err := store.FailUnfinishedHooks(ctx, tx, run.ID, activeHook, problem); err != nil {
			return err
		}
		if run.AgentStatus != nil {
			status, agentProblem := *run.AgentStatus, run.AgentError
			if status == session.Completed {
				status, agentProblem = session.Failed, problem
			}
			return store.Finish(ctx, tx, r, run, status, agentProblem, run.StopMethod)
		}
		if run.ExecutionStartedAt != nil {
			run.AgentStatus = new(session.Failed)
			run.AgentError = problem
		}
		return store.Finish(ctx, tx, r, run, session.Failed, problem, nil)
	})
}
func (e *Executor) Disconnect() {
	if e.driver != nil {
		_ = e.driver.Close()
		e.driver = nil
	}
	if e.account != nil {
		_ = e.account.Close()
		e.account = nil
	}
	e.sandbox = nil
	e.runnerReady = false
}
func (e *Executor) Run(ctx context.Context) {
	defer e.Disconnect()
	for ctx.Err() == nil {
		err := e.Tick(ctx)
		if err != nil {
			if f, ok := errors.AsType[*harness.ExecutionError](err); ok {
				err = e.failure(ctx, f)
			} else {
				observation := "reconnecting"
				if errors.Is(err, harness.ErrUncertain) {
					observation = "uncertain"
				}
				_ = e.observation(ctx, observation)
				e.Disconnect()
				slog.WarnContext(ctx, "Session observation interrupted", "session_id", e.ID, "error_type", diagnostic.Describe(err))
			}
			if err != nil && ctx.Err() == nil {
				slog.WarnContext(ctx, "Session tick failed", "session_id", e.ID, "error_type", diagnostic.Describe(err))
			}
		}
		record, _, readErr := e.read(ctx)
		if readErr == nil && !record.SlotReserved {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(e.Store.Settings.WorkerPoll):
		}
	}
}
func (e *Executor) Tick(ctx context.Context) error {
	err := e.tick(ctx)
	if err == nil {
		return nil
	}
	if errors.Is(err, harness.ErrNotFound) {
		r, _, readErr := e.read(ctx)
		if readErr != nil {
			return readErr
		}
		if r.SandboxID == nil {
			return harness.Failure("environment_unavailable", "Sandbox template is unavailable.")
		}
		_, infoErr := e.Platform.Info(ctx, *r.SandboxID)
		if errors.Is(infoErr, harness.ErrNotFound) {
			return e.failure(ctx, &harness.ExecutionError{Code: "sandbox_lost", Message: "Sandbox is no longer available."})
		}
		return err
	}
	if errors.Is(err, harness.ErrRejected) {
		_, run, readErr := e.read(ctx)
		if readErr != nil {
			return readErr
		}
		if run != nil && run.ExecutionStartedAt != nil {
			return err
		}
		if errors.Is(err, harness.ErrEnvironmentRejected) {
			return harness.Failure("environment_unavailable", "Sandbox environment was rejected.")
		}
		return harness.Failure("harness_failed", "Harness rejected initialization.")
	}
	return err
}
func (e *Executor) tick(ctx context.Context) error {
	record, run, err := e.read(ctx)
	if err != nil {
		return err
	}
	if run == nil {
		return e.pause(ctx, record)
	}
	if run.Status == session.Accepted {
		if err := e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, r *store.SessionRecord) error {
			current, err := store.GetRun(ctx, tx, e.ID, run.ID)
			if err != nil {
				return err
			}
			if current.Status == session.Accepted {
				current.Status = session.Starting
				current.Phase = new("preparation")
				return store.PublishRun(ctx, tx, r, &current)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	record, run, err = e.read(ctx)
	if err != nil {
		return err
	}
	if run == nil {
		return e.pause(ctx, record)
	}
	if run.CancelRequestedAt != nil && run.ExecutionStartedAt == nil {
		if run.Phase != nil && (*run.Phase == "after_create" || *run.Phase == "before_run") {
			h, err := e.Store.Hook(ctx, run.ID, *run.Phase)
			if err != nil {
				return err
			}
			if h != nil && h.Status == "running" {
				if err := e.ensureSandbox(ctx, &record, run); err != nil {
					return err
				}
				if err := e.preparePaths(ctx, &record); err != nil {
					return err
				}
				done, _, err := e.runHook(ctx, record, *run, *run.Phase)
				if err != nil || !done {
					return err
				}
			}
		}
		if err := e.skipHooks(ctx, *run, "after_create", "before_run", "after_run"); err != nil {
			return err
		}
		if record.SandboxID == nil {
			attempt, err := e.Store.LatestAttempt(ctx, e.ID, "sandbox_create")
			if err != nil {
				return err
			}
			if attempt != nil {
				if err := e.ensureSandbox(ctx, &record, run); err != nil {
					return err
				}
			}
		}
		return e.cancelRun(ctx, record, *run)
	}
	if err := e.ensureSandbox(ctx, &record, run); err != nil {
		return err
	}
	if run.Status == session.Finalizing {
		if err := e.preparePaths(ctx, &record); err != nil {
			return err
		}
		done, hookError, err := e.runHook(ctx, record, *run, "after_run")
		if err != nil || !done {
			return err
		}
		if run.AgentStatus == nil {
			return harness.ErrUncertain
		}
		status, problem := *run.AgentStatus, run.AgentError
		if status == session.Completed && hookError != nil {
			status, problem = session.Failed, hookError
		}
		return e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, r *store.SessionRecord) error {
			current, err := store.GetRun(ctx, tx, e.ID, run.ID)
			if err != nil {
				return err
			}
			return store.Finish(ctx, tx, r, &current, status, problem, current.StopMethod)
		})
	}
	if run.ExecutionStartedAt == nil {
		if err := e.preparePaths(ctx, &record); err != nil {
			return err
		}
		for _, name := range []string{"after_create", "before_run"} {
			done, problem, err := e.runHook(ctx, record, *run, name)
			if err != nil || !done {
				return err
			}
			if problem != nil {
				return e.finishPreparationError(ctx, *run, name, problem)
			}
		}
	}
	if run.DeadlineAt != nil && !time.Now().Before(*run.DeadlineAt) && run.CancelRequestedAt == nil {
		if err := e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, r *store.SessionRecord) error {
			current, err := store.GetRun(ctx, tx, e.ID, run.ID)
			if err != nil {
				return err
			}
			if current.Status.Terminal() || current.CancelRequestedAt != nil {
				return nil
			}
			current.CancelRequestedAt = new(time.Now().UTC())
			current.StopReason = new("run_timeout")
			current.Status = session.Cancelling
			return store.PublishRun(ctx, tx, r, &current)
		}); err != nil {
			return err
		}
		record, run, err = e.read(ctx)
		if err != nil {
			return err
		}
		if run == nil {
			return nil
		}
	}
	if run.CancelRequestedAt != nil {
		if run.CancelAttemptedAt == nil {
			if err := e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, r *store.SessionRecord) error {
				current, err := store.GetRun(ctx, tx, e.ID, run.ID)
				if err != nil {
					return err
				}
				if current.CancelAttemptedAt == nil {
					current.CancelAttemptedAt = new(time.Now().UTC())
				}
				run.CancelAttemptedAt = current.CancelAttemptedAt
				return store.PublishRun(ctx, tx, r, &current)
			}); err != nil {
				return err
			}
		}
		if !time.Now().Before(run.CancelAttemptedAt.Add(e.Store.Settings.CancelGrace)) {
			if err := e.forceStop(ctx, record, *run); err != nil {
				return err
			}
			return e.ensureHarness(ctx, &record, run)
		}
	}
	if err := e.ensureHarness(ctx, &record, run); err != nil {
		return err
	}
	if e.driver == nil {
		return nil
	}
	record, run, err = e.read(ctx)
	if err != nil || run == nil {
		return err
	}
	if run.ExecutionStartedAt != nil || run.Number > 1 {
		if err := e.refresh(ctx, record); err != nil {
			return err
		}
	}
	record, run, err = e.read(ctx)
	if err != nil || run == nil {
		return err
	}
	if run.Status == session.Finalizing {
		return nil
	}
	if run.CancelRequestedAt != nil {
		err = e.cancelRun(ctx, record, *run)
	} else {
		err = e.send(ctx, record, *run)
	}
	if err != nil {
		return err
	}
	if e.account != nil {
		e.account.Sync(ctx, false)
	}
	return nil
}
func Run(ctx context.Context, s *store.Store, platform harness.Platform) error {
	// pgx.Connect creates a dedicated physical connection, never a pool checkout
	// that can be transparently replaced while the lock is assumed to be held.
	owner, err := pgx.Connect(ctx, s.Settings.DatabaseURL)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = owner.Close(closeCtx)
	}()
	owned, err := store.TryWorkerLock(ctx, owner)
	if err != nil {
		return err
	}
	if !owned {
		return errors.New("another worker owns the executor lock")
	}
	execution, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	tasks := map[uuid.UUID]<-chan struct{}{}
	for ctx.Err() == nil {
		probe, cancelProbe := context.WithTimeout(ctx, s.Settings.ReadinessTimeout)
		err := owner.Ping(probe)
		cancelProbe()
		if err != nil {
			return err
		}
		ids, err := s.Reserved(ctx)
		if err != nil {
			return err
		}
		for id, done := range tasks {
			select {
			case <-done:
				delete(tasks, id)
			default:
			}
		}
		for _, id := range ids {
			if _, ok := tasks[id]; !ok {
				done := make(chan struct{})
				tasks[id] = done
				wg.Go(func() { defer close(done); NewExecutor(id, s, platform).Run(execution) })
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(s.Settings.WorkerPoll):
		}
	}
	return nil
}
