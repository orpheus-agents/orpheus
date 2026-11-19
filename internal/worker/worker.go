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
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/credentials"
	"github.com/orpheus-agents/orpheus/internal/diagnostic"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/harness/codex"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

type DriverFactory func(harness.Sandbox, store.SessionRecord, uuid.UUID, session.Credentials) harness.Driver

var errStoreAccess = errors.New("session database access failed")

type AccountFactory func(context.Context, harness.Sandbox, string, session.Credentials) (credentials.Sync, error)
type Executor struct {
	ID               uuid.UUID
	Store            *store.Store
	Platform         harness.Platform
	NewDriver        DriverFactory
	NewAccount       AccountFactory
	RunnerBinary     func(string) ([]byte, error)
	Limits           *AccountLimitsCollector
	unregisterLimits func()
	sandbox          harness.Sandbox
	driverRunID      uuid.UUID
	driver           harness.Driver
	account          credentials.Sync
	snapshot         harness.Snapshot
	settleRetryAt    time.Time
	settleRetryDelay time.Duration
	settleAuthSynced bool
	timeoutRenewAt   time.Time
	timeoutRunID     uuid.UUID
	runnerReady      bool
	hookTermSent     uuid.UUID
	clock            func() time.Time
	redactor         *diagnostic.Redactor
}

func NewExecutor(id uuid.UUID, s *store.Store, p harness.Platform) *Executor {
	e := &Executor{ID: id, Store: s, Platform: p, RunnerBinary: os.ReadFile, settleRetryDelay: 5 * time.Second, redactor: new(diagnostic.Redactor)}
	e.NewDriver = func(box harness.Sandbox, record store.SessionRecord, run uuid.UUID, source session.Credentials) harness.Driver {
		if record.Configuration.Harness == "opencode" {
			return e.consoleDriver(record, run, source)
		}
		return codex.New(box, s.Settings.RPCTimeout, s.Settings.MaxToolResultBytes, e.redactor)
	}
	e.NewAccount = func(ctx context.Context, box harness.Sandbox, home string, source session.Credentials) (credentials.Sync, error) {
		return credentials.New(ctx, box, home, source, e.redactor)
	}
	return e
}
func (e *Executor) read(ctx context.Context) (store.SessionRecord, *store.RunRecord, error) {
	r, run, err := e.Store.Read(ctx, e.ID)
	if err != nil {
		err = errors.Join(errStoreAccess, err)
	}
	return r, run, err
}
func (e *Executor) state(ctx context.Context, state string, problem *session.Error) error {
	return e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, r *store.SessionRecord) error {
		if r.SandboxState == "unavailable" && state != "unavailable" && (r.AllowMultipleRuns || state != "deleting") {
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
	Adopted     bool    `json:"adopted,omitzero"`
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
	if status == "sending" {
		blocked := false
		err := e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, _ *store.SessionRecord) error {
			o, err := db.New(tx).GetSessionOperation(ctx, db.GetSessionOperationParams{ID: id, SessionID: e.ID})
			if err != nil {
				return err
			}
			if o.Kind == "launch" && o.RunID != nil {
				run, err := store.GetRun(ctx, tx, e.ID, *o.RunID)
				if err != nil {
					return err
				}
				if run.Status.Terminal() || run.CancelRequestedAt != nil {
					blocked = true
					return nil
				}
			}
			o.Status = status
			if o.AttemptedAt == nil {
				o.AttemptedAt = new(time.Now().UTC())
			}
			return store.SaveOperation(ctx, tx, &o)
		})
		if err != nil {
			return err
		}
		if blocked {
			return errProvisionWait
		}
		return nil
	}
	return e.Store.SetOperation(ctx, e.ID, id, status, result)
}

func (e *Executor) invoke(ctx context.Context, o store.Operation, call func() (operationResult, error)) (result operationResult, err error) {
	defer func() {
		if err != nil {
			fields := diagnostic.Capture(err)
			fields.OperationID = &o.ID
			err = diagnostic.With(err, fields)
		}
	}()
	if o.Status == "confirmed" {
		return decodeResult(o)
	}
	if o.Status != "pending" {
		return operationResult{}, harness.ErrUncertain
	}
	if err := e.opStatus(ctx, o.ID, "sending", nil); err != nil {
		return operationResult{}, err
	}
	result, err = call()
	if err != nil {
		_, executionFailure := errors.AsType[*harness.ExecutionError](err)
		if errors.Is(err, harness.ErrRejected) || errors.Is(err, harness.ErrNotFound) || executionFailure {
			if saveErr := e.opStatus(ctx, o.ID, "failed", nil); saveErr != nil {
				return result, diagnostic.With(saveErr, diagnostic.Capture(err))
			}
			if errors.Is(err, harness.ErrEnvironmentRejected) {
				return result, harness.FailureCause("environment_unavailable", "Sandbox operation was rejected.", err)
			}
			return result, err
		}
		if saveErr := e.opStatus(ctx, o.ID, "uncertain", nil); saveErr != nil {
			return result, diagnostic.With(saveErr, diagnostic.Capture(err))
		}
		return result, errors.Join(harness.ErrUncertain, err)
	}
	if err := e.opStatus(ctx, o.ID, "confirmed", result); err != nil {
		return result, err
	}
	return result, nil
}
func (e *Executor) failure(ctx context.Context, f *harness.ExecutionError) error {
	fields := diagnostic.Capture(f)
	if fields.SessionID == uuid.Nil {
		record, run, err := e.read(ctx)
		if err != nil {
			return err
		}
		fields = diagnostic.Capture(e.bindFailure(f, record, run))
	}
	fields.PublicCode = f.Code
	if ctx.Err() == nil {
		diagnostic.Log(ctx, slog.LevelError, fields)
	}
	unavailable := f.Code == "sandbox_lost" || f.Code == "context_lost"
	err := e.Store.Mutate(ctx, e.ID, unavailable, func(tx pgx.Tx, r *store.SessionRecord) error {
		var run *store.RunRecord
		if fields.RunID != nil {
			current, err := store.GetRun(ctx, tx, e.ID, *fields.RunID)
			if err != nil {
				return err
			}
			if current.Status.Terminal() {
				return nil
			}
			run = &current
		}
		// Admission must see termination and loss of the environment atomically.
		// Otherwise a new run can enter between Finish and the unavailable state.
		if unavailable {
			if err := store.UpdateSandbox(ctx, tx, r, func(r *store.SessionRecord) {
				r.SandboxState = "unavailable"
				r.SandboxLastKnownState = new("unavailable")
				r.SandboxError = &session.Error{Code: f.Code, Message: f.Message, Phase: new("recovery"), Details: []session.Detail{}, DiagnosticID: &fields.ID}
				if f.Code == "sandbox_lost" {
					r.SlotReserved = false
				}
			}); err != nil {
				return err
			}
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
		problem := &session.Error{Code: f.Code, Message: f.Message, Phase: &phase, Details: []session.Detail{}, DiagnosticID: &fields.ID}
		if run.ExecutionStartedAt != nil && run.AgentStatus == nil && r.Configuration.Public.Hooks.AfterRun != nil &&
			f.Code != "sandbox_lost" && f.Code != "environment_unavailable" {
			return store.AgentFinished(ctx, tx, r, run, session.Failed, problem, nil)
		}
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
	return diagnostic.With(err, fields)
}
func (e *Executor) Disconnect() {
	if e.unregisterLimits != nil {
		e.unregisterLimits()
		e.unregisterLimits = nil
	}
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
	e.hookTermSent = uuid.Nil
	e.redactor = new(diagnostic.Redactor)
}
func (e *Executor) Run(ctx context.Context) {
	defer e.Disconnect()
	for ctx.Err() == nil {
		err := e.Tick(ctx)
		if err != nil {
			if f, ok := errors.AsType[*harness.ExecutionError](err); ok {
				if saveErr := e.failure(ctx, f); saveErr != nil && ctx.Err() == nil {
					slog.WarnContext(ctx, "Execution failure could not be saved", "session_id", e.ID, "diagnostic_id", diagnostic.Capture(saveErr).ID, "error_type", diagnostic.Describe(saveErr))
				}
			} else if !errors.Is(err, errStoreAccess) {
				if ctx.Err() == nil {
					diagnostic.Log(ctx, slog.LevelWarn, diagnostic.Capture(err))
				}
				observation := "reconnecting"
				if errors.Is(err, harness.ErrUncertain) {
					observation = "uncertain"
				}
				_ = e.observation(ctx, observation)
				e.Disconnect()
			} else if ctx.Err() == nil {
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
	if errors.Is(err, errProvisionWait) {
		return nil
	}
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*harness.ExecutionError](err); ok {
		return err
	}
	if errors.Is(err, harness.ErrNotFound) {
		r, _, readErr := e.read(ctx)
		if readErr != nil {
			return readErr
		}
		if r.SandboxID == nil {
			return harness.FailureCause("environment_unavailable", "Sandbox template is unavailable.", err)
		}
		_, infoErr := e.Platform.Info(ctx, *r.SandboxID)
		if errors.Is(infoErr, harness.ErrNotFound) {
			return harness.FailureCause("sandbox_lost", "Sandbox is no longer available.", err)
		}
		return err
	}
	if errors.Is(err, harness.ErrRejected) {
		if fields := diagnostic.Capture(err); fields.Phase == "execution" || fields.Phase == "finalization" {
			return err
		}
		if errors.Is(err, harness.ErrEnvironmentRejected) {
			return harness.FailureCause("environment_unavailable", "Sandbox environment was rejected.", err)
		}
		return harness.FailureCause("harness_failed", "Harness rejected initialization.", err)
	}
	return err
}

func (e *Executor) finalizeRun(ctx context.Context, record store.SessionRecord, run store.RunRecord) error {
	if err := e.preparePaths(ctx, &record); err != nil {
		return err
	}
	done, hookError, err := e.runHook(ctx, record, run, "after_run")
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

func (e *Executor) tick(ctx context.Context) (err error) {
	record, run, err := e.read(ctx)
	defer func() { err = e.bindFailure(err, record, run) }()
	if err != nil {
		return err
	}
	if run == nil {
		if !record.SlotReserved {
			return nil
		}
		return e.settleEnvironment(ctx, record)
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
		if !record.SlotReserved {
			return nil
		}
		return e.settleEnvironment(ctx, record)
	}
	if store.BudgetExhausted(record) && run.CancelRequestedAt == nil && run.AgentStatus == nil && run.Status != session.Finalizing {
		if err := e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, r *store.SessionRecord) error {
			current, err := store.GetRun(ctx, tx, e.ID, run.ID)
			if err != nil {
				return err
			}
			return store.EnforceTokenBudget(ctx, tx, r, &current)
		}); err != nil {
			return err
		}
		record, run, err = e.read(ctx)
		if err != nil || run == nil {
			return err
		}
	}
	if _, err := e.provision(ctx, run.ID, false, nil); err != nil {
		return err
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

		return e.cancelRun(ctx, record, *run)
	}
	if err := e.ensureSandbox(ctx, &record, run); err != nil {
		return err
	}
	record, run, err = e.read(ctx)
	if err != nil || run == nil {
		return err
	}
	if run.ExecutionStartedAt == nil && run.CancelRequestedAt != nil {
		return nil
	}
	if run.Status == session.Finalizing {
		return e.finalizeRun(ctx, record, *run)
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
	if record.ThreadID == nil {
		return harness.Failure("harness_failed", "Native thread is unavailable.")
	}
	dispatched, err := e.Store.ThreadHasDispatch(ctx, e.ID, *record.ThreadID)
	if errors.Is(err, store.ErrThreadDispatchInvalid) {
		return harness.Failure("harness_failed", "Cannot determine assignment delivery state.")
	}
	if err != nil {
		return err
	}
	if dispatched {
		if err := e.refresh(ctx, record); err != nil {
			return err
		}
	} else {
		e.snapshot = harness.Snapshot{}
	}
	record, run, err = e.read(ctx)
	if err != nil || run == nil {
		return err
	}
	if run.Status == session.Finalizing {
		return e.finalizeRun(ctx, record, *run)
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
	return run(ctx, s, platform, store.CleanupBrowserAuth)
}

func run(ctx context.Context, s *store.Store, platform harness.Platform, cleanup func(context.Context, *db.Queries)) error {
	// pgx.Connect creates a dedicated physical connection, never a pool checkout
	// that can be transparently replaced while the lock is assumed to be held.
	ownerConfig, err := config.DatabaseConnectionConfig(s.Settings.DatabaseURL)
	if err != nil {
		return err
	}
	owner, err := pgx.ConnectConfig(ctx, ownerConfig)
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
	if err := s.RecoverProvisionReservations(ctx); err != nil {
		return err
	}
	slog.InfoContext(ctx, "Worker started")
	defer slog.InfoContext(ctx, "Worker stopped")
	execution, cancel := context.WithCancel(ctx)
	collector := newAccountLimitsCollector(execution, s)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait(); collector.Wait() }()
	// Purge expired identities even after SAML is disabled, under worker ownership.
	wg.Go(func() { cleanup(execution, db.New(s.Pool)) })
	tasks := map[uuid.UUID]workerTask{}
	var retention sandboxCleanupPass
	for ctx.Err() == nil {
		probe, cancelProbe := context.WithTimeout(ctx, s.Settings.DatabasePingTimeout)
		err := owner.Ping(probe)
		cancelProbe()
		if err != nil {
			return err
		}
		// Reap tasks before reading reservations so a stale reservation cannot
		// restart an executor that released its slot during the query.
		for id, task := range tasks {
			select {
			case <-task.done:
				if task.cleanupResult != nil {
					retention.finished(*task.cleanupResult)
				}
				delete(tasks, id)
			default:
			}
		}
		ids, err := s.Reserved(ctx)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if _, ok := tasks[id]; !ok {
				done := make(chan struct{})
				tasks[id] = workerTask{done: done}
				wg.Go(func() {
					defer close(done)
					executor := NewExecutor(id, s, platform)
					executor.Limits = collector
					executor.Run(execution)
				})
			}
		}
		if err := retention.advance(execution, s, platform, tasks, &wg, time.Now().UTC()); err != nil && execution.Err() == nil {
			slog.WarnContext(ctx, "Paused sandbox cleanup selection failed", "error_type", diagnostic.Describe(err))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(s.Settings.WorkerPoll):
		}
	}
	return nil
}
