package worker

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/credentials"
	"github.com/orpheus-agents/orpheus/internal/diagnostic"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/harness/opencode"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

// Renew a short lease independently of run and hook deadlines. Requesting the
// entire run plus preparation time can exceed the AgentBox plan's timeout limit.
// AgentBox still enforces its maximum uninterrupted sandbox lifetime.
const sandboxLeaseTimeout = 5 * time.Minute

func (e *Executor) ensureSandbox(ctx context.Context, record *store.SessionRecord, run *store.RunRecord) error {
	o, err := e.provision(ctx, run.ID, true, nil)
	if err != nil {
		return err
	}
	if o != nil {
		return e.ensureProvision(ctx, record, run, *o)
	}
	if record.SandboxID == nil {
		return harness.ErrUncertain
	}

	switch {
	case e.sandbox == nil:
		state, err := e.Platform.Info(ctx, *record.SandboxID)
		if err != nil {
			return err
		}
		if state == "paused" {
			if err := e.state(ctx, "resuming", nil); err != nil {
				return err
			}
		}
		o, err := e.operation(ctx, "resume", &run.ID, nil, nil)
		if err != nil {
			return err
		}
		// A confirmed initial resume stays confirmed across later reconnects.
		if o.Status != "confirmed" {
			if err := e.opStatus(ctx, o.ID, "sending", nil); err != nil {
				return err
			}
		}
		box, err := e.Platform.Connect(ctx, *record.SandboxID, sandboxLeaseTimeout)
		if err != nil {
			return err
		}
		e.sandbox = box
		if err := e.opStatus(ctx, o.ID, "confirmed", nil); err != nil {
			return err
		}
	case e.timeoutRunID != run.ID || !time.Now().Before(e.timeoutRenewAt):
		if err := e.sandbox.SetTimeout(ctx, sandboxLeaseTimeout); err != nil {
			return err
		}
	default:
		return nil
	}
	e.timeoutRunID = run.ID
	e.timeoutRenewAt = time.Now().Add(time.Minute)
	e.settleAuthSynced = false
	e.settleRetryAt = time.Time{}
	e.settleRetryDelay = 5 * time.Second
	return e.state(ctx, "ready", nil)
}
func (e *Executor) preparePaths(ctx context.Context, record *store.SessionRecord) error {
	if record.Workspace != nil {
		return nil
	}
	output, err := e.sandbox.Run(ctx, `set -eu; w="$HOME/workspace"; mkdir -p "$w"; printf '%s' "$w"`)
	if err != nil {
		return err
	}
	workspace := string(output)
	if !strings.HasPrefix(workspace, "/") || strings.ContainsAny(workspace, "\x00\r\n") {
		return harness.Failure("environment_unavailable", "Sandbox paths are unavailable.")
	}
	record.Workspace = &workspace
	return e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, r *store.SessionRecord) error {
		return store.UpdateSandbox(ctx, tx, r, func(r *store.SessionRecord) {
			r.Workspace = record.Workspace
		})
	})
}
func matching(processes []harness.Process, pid *int, launch *uuid.UUID) *harness.Process {
	if pid == nil || launch == nil {
		return nil
	}
	for i := range processes {
		p := &processes[i]
		if p.PID == *pid && p.Env["ORPHEUS_LAUNCH_ID"] == launch.String() {
			return p
		}
	}
	return nil
}
func launchMatches(processes []harness.Process, id uuid.UUID) []harness.Process {
	return slices.DeleteFunc(slices.Clone(processes), func(p harness.Process) bool { return p.Env["ORPHEUS_LAUNCH_ID"] != id.String() })
}
func (e *Executor) ensureHarness(ctx context.Context, record *store.SessionRecord, run *store.RunRecord) error {
	cfg := record.Configuration
	source, err := e.Store.SessionCredentials(ctx, *record)
	if err != nil {
		if !errors.Is(err, store.ErrCredentialsUnavailable) {
			return errors.Join(errStoreAccess, err)
		}
		return harness.Failure("credentials_unavailable", "Account credentials are unavailable.")
	}
	e.redactor.Add(os.Getenv("AGENTBOX_API_KEY"), source.APIKey)
	repair := false
	if run.ExecutionStartedAt == nil {
		var err error
		repair, err = e.Store.AuthFailedBefore(ctx, e.ID, run.Number, false)
		if err != nil {
			return err
		}
	}
	isOpenCode := cfg.Harness == "opencode"
	rotating := isOpenCode && e.driver != nil && e.driverRunID != run.ID
	if e.driver != nil && !repair && !rotating {
		return e.bindHarness(ctx, run, record.LaunchID)
	}
	if e.driver != nil {
		if isOpenCode && e.account != nil {
			if err := flushAccount(ctx, e.account); err != nil {
				return err
			}
			_ = e.account.Close()
			e.account = nil
		}
		if e.unregisterLimits != nil {
			e.unregisterLimits()
			e.unregisterLimits = nil
		}
		_ = e.driver.Close()
		e.driver = nil
	}
	if repair && e.account != nil {
		_ = e.account.Close()
		e.account = nil
	}
	if err := e.preparePaths(ctx, record); err != nil {
		return err
	}
	processes, err := e.sandbox.Processes(ctx)
	if err != nil {
		return err
	}
	process := matching(processes, record.ProcessID, record.LaunchID)
	if process == nil {
		outstanding, err := e.Store.LatestAttempt(ctx, e.ID, "launch")
		if err != nil {
			return err
		}
		if outstanding != nil && (record.LaunchID == nil || outstanding.ID != *record.LaunchID) {
			matches := launchMatches(processes, outstanding.ID)
			if len(matches) > 1 || (len(matches) == 0 && outstanding.Status != "confirmed") {
				return harness.ErrUncertain
			}
			if len(matches) == 1 {
				process = &matches[0]
				if err := e.opStatus(ctx, outstanding.ID, "confirmed", operationResult{PID: process.PID}); err != nil {
					return err
				}
				record.ProcessID = &process.PID
				record.LaunchID = &outstanding.ID
				if err := e.Store.Mutate(ctx, e.ID, false, func(_ pgx.Tx, r *store.SessionRecord) error {
					r.ProcessID = record.ProcessID
					r.LaunchID = record.LaunchID
					return nil
				}); err != nil {
					return err
				}
			}
		}
	}
	var launch *store.Operation
	if record.LaunchID != nil {
		launch, err = e.Store.GetOperation(ctx, *record.LaunchID)
		if err != nil {
			return err
		}
	}
	repair = repair && (launch == nil || launch.RunID == nil || *launch.RunID != run.ID)
	previousRun := isOpenCode && launch != nil && launch.RunID != nil && *launch.RunID != run.ID
	if previousRun && process != nil && source.Mode == "account" && !rotating {
		if err := e.flushSavedConsole(ctx, *record, *launch.RunID, source, process); err != nil {
			return err
		}
	}
	if (repair || previousRun) && process != nil {
		return e.sandbox.Kill(ctx, process.PID)
	}
	driver := e.NewDriver(e.sandbox, *record, run.ID, source)
	adopted := false
	var unregisterLimits func()
	defer func() {
		if !adopted {
			if unregisterLimits != nil {
				unregisterLimits()
			}
			_ = driver.Close()
		}
	}()
	if e.account == nil && source.Mode == "account" {
		if record.HarnessHome == nil {
			home, err := driver.StateDir(ctx)
			if err != nil {
				return err
			}
			record.HarnessHome = &home
			if err := e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, r *store.SessionRecord) error {
				return store.UpdateSandbox(ctx, tx, r, func(r *store.SessionRecord) {
					r.HarnessHome = record.HarnessHome
				})
			}); err != nil {
				return err
			}
		}
		if !isOpenCode {
			e.account, err = e.NewAccount(ctx, e.sandbox, *record.HarnessHome, source)
		}
		if err != nil {
			return harness.Failure("credentials_unavailable", "Account credentials are unavailable.")
		}
	}
	if process == nil {
		if run.ExecutionStartedAt != nil {
			return e.recoverDead(ctx, *record, *run, driver)
		}
		o, err := e.operation(ctx, "launch", &run.ID, nil, nil)
		if err != nil {
			return err
		}
		pid := 0
		if o.Status == "sending" || o.Status == "uncertain" || o.Status == "confirmed" {
			matches := launchMatches(processes, o.ID)
			if len(matches) == 0 && o.Status == "confirmed" {
				return harness.Failure("harness_failed", "Harness exited during preparation.")
			}
			if len(matches) != 1 {
				return harness.ErrUncertain
			}
			pid = matches[0].PID
			e.rememberProcessEnvironment(matches[0])
			if err := driver.Attach(ctx, pid); err != nil {
				return err
			}
			if err := e.opStatus(ctx, o.ID, "confirmed", operationResult{PID: pid}); err != nil {
				return err
			}
		} else {
			env, err := e.Store.SessionEnvironment(ctx, e.Store.Pool, *record)
			if err != nil {
				if !errors.Is(err, store.ErrEnvironmentUnavailable) {
					return errors.Join(errStoreAccess, err)
				}
				return harness.Failure("environment_unavailable", "Harness environment is unavailable.")
			}
			env, err = e.Store.Deployment.ResolveEnvironment(config.EnvironmentInput{Services: cfg.Public.Sandbox.Services, EnvFrom: cfg.Public.Sandbox.EnvFrom, Env: env}, config.EnvironmentInput{}, e.Store.Settings.HarnessEnvAllowlist)
			if err != nil {
				return harness.Failure("environment_unavailable", err.Error())
			}
			if !isOpenCode && e.account != nil {
				// A replacement process must not inherit credentials that became
				// stale while its sandbox was paused or disconnected.
				if err := e.account.Seed(ctx); err != nil {
					return err
				}
			}
			config.AddSandboxProxy(env, e.Store.Settings.SandboxProxyURL)
			env["ORPHEUS_LAUNCH_ID"] = o.ID.String()
			result, err := e.invoke(ctx, o, slices.Sorted(maps.Keys(env)), func() (operationResult, error) {
				pid, err := driver.Launch(ctx, env, *record.Workspace, source)
				return operationResult{PID: pid}, err
			})
			if err != nil {
				return err
			}
			pid = result.PID
		}
		record.ProcessID = &pid
		record.LaunchID = &o.ID
		if err := e.Store.Mutate(ctx, e.ID, false, func(_ pgx.Tx, r *store.SessionRecord) error {
			r.ProcessID = record.ProcessID
			r.LaunchID = record.LaunchID
			return nil
		}); err != nil {
			return err
		}
	} else {
		e.rememberProcessEnvironment(*process)
		if err := driver.Attach(ctx, process.PID); err != nil {
			return err
		}
	}
	launch, err = e.Store.GetOperation(ctx, *record.LaunchID)
	if err != nil {
		return err
	}
	initialized := false
	if launch != nil {
		result, err := decodeResult(*launch)
		if err != nil {
			return err
		}
		initialized = result.Initialized
	}
	if err := driver.Initialize(ctx, source, !initialized); err != nil {
		return err
	}
	if isOpenCode && source.Mode == "account" {
		account, err := credentials.NewOpenCode(ctx, e.sandbox, *record.HarnessHome, driver.(credentials.ConsoleClient), source, e.redactor)
		if err != nil {
			return harness.FailureCause("credentials_unavailable", "Cannot open OpenCode account credentials.", err)
		}
		if err = account.Seed(ctx); err != nil {
			_ = account.Close()
			return harness.FailureCause("credentials_unavailable", "Cannot synchronize OpenCode account credentials.", err)
		}
		e.account = account
		defer func() {
			if !adopted {
				_ = account.Close()
				e.account = nil
			}
		}()
	}
	if e.Limits != nil {
		unregisterLimits = e.Limits.Register(e.ID, cfg.Harness, source, driver)
	}
	if record.ThreadID != nil {
		if !initialized {
			if _, err := driver.OpenContext(ctx, cfg.Public.Agent, *record.Workspace, record.ThreadID); err != nil {
				return err
			}
		}
	} else {
		o, err := e.operation(ctx, "thread", &run.ID, nil, nil)
		if err != nil {
			return err
		}
		var result operationResult
		if o.Status == "confirmed" {
			result, err = decodeResult(o)
			if err != nil {
				return err
			}
		} else {
			if err := e.opStatus(ctx, o.ID, "sending", nil); err != nil {
				return err
			}
			native, err := driver.OpenContext(ctx, cfg.Public.Agent, *record.Workspace, nil)
			if err != nil {
				return err
			}
			result = operationResult{ID: native.NativeID, Path: native.HistoryPath}
			if err := e.opStatus(ctx, o.ID, "confirmed", result); err != nil {
				return err
			}
		}
		record.ThreadID = &result.ID
		record.HistoryPath = result.Path
		if err := e.Store.Mutate(ctx, e.ID, false, func(_ pgx.Tx, r *store.SessionRecord) error {
			r.ThreadID = record.ThreadID
			r.HistoryPath = record.HistoryPath
			return nil
		}); err != nil {
			return err
		}
	}
	if !initialized {
		if err := e.opStatus(ctx, *record.LaunchID, "confirmed", operationResult{PID: *record.ProcessID, Initialized: true}); err != nil {
			return err
		}
	}
	if e.account != nil {
		if err := e.account.Watch(ctx); err != nil {
			return err
		}
	}
	if err := e.bindHarness(ctx, run, record.LaunchID); err != nil {
		return err
	}
	e.driver = driver
	e.driverRunID = run.ID
	e.unregisterLimits = unregisterLimits
	adopted = true
	return nil
}

func (e *Executor) rememberProcessEnvironment(process harness.Process) {
	values := make([]string, 0, len(process.Env))
	for _, value := range process.Env {
		values = append(values, value)
	}
	e.redactor.Add(values...)
}
func (e *Executor) recoverDead(ctx context.Context, record store.SessionRecord, run store.RunRecord, driver harness.Driver) error {
	snapshot, err := driver.Recover(ctx, record.ThreadID, record.HistoryPath, record.HarnessHome)
	if err != nil {
		return err
	}
	return e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, r *store.SessionRecord) error {
		if err := store.Reconcile(ctx, tx, r, snapshot); err != nil {
			return err
		}
		if err := store.ApplyUsage(ctx, tx, r, snapshot.Usage); err != nil {
			return err
		}
		current, err := store.GetRun(ctx, tx, e.ID, run.ID)
		if err != nil {
			return err
		}
		if current.Status.Terminal() || current.AgentStatus != nil {
			return nil
		}
		if current.CancelRequestedAt != nil {
			return store.AgentFinished(ctx, tx, r, &current, session.Cancelled, nil, new("forced"))
		}
		return store.AgentFinished(ctx, tx, r, &current, session.Failed, &session.Error{Code: "harness_failed", Message: "Harness exited before completion.", Phase: new("execution"), Details: []session.Detail{}}, nil)
	})
}
func (e *Executor) refresh(ctx context.Context, record store.SessionRecord) error {
	if !e.driver.HasUpdates() {
		return nil
	}
	snapshot, err := e.driver.Snapshot(ctx, *record.ThreadID, record.HistoryPath, record.HistoryOffset)
	if err != nil {
		return err
	}
	e.snapshot = snapshot
	if err := e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, r *store.SessionRecord) error {
		if err := store.Reconcile(ctx, tx, r, snapshot); err != nil {
			return err
		}
		return store.ApplyUsage(ctx, tx, r, snapshot.Usage)
	}); err != nil {
		return errors.Join(errStoreAccess, err)
	}
	e.driver.Committed()
	return nil
}
func (e *Executor) settleEnvironment(ctx context.Context, record store.SessionRecord) error {
	outstanding, err := openCreate(ctx, e.Store.Pool, e.ID)
	if err != nil {
		return err
	}
	if outstanding != nil {
		return e.settleProvision(ctx, *outstanding)
	}

	if time.Now().Before(e.settleRetryAt) {
		return nil
	}
	err = e.settleEnvironmentNow(ctx, record)
	if err == nil {
		e.settleRetryAt = time.Time{}
		e.settleRetryDelay = 5 * time.Second
		return nil
	}
	if errors.Is(err, harness.ErrNotFound) {
		return err
	}
	state, code, message := "pausing", "environment_unavailable", "Sandbox pause failed; retrying."
	if !record.AllowMultipleRuns {
		state, code, message = "deleting", "sandbox_delete_failed", "Sandbox deletion failed; retrying."
	}
	if stateErr := e.state(ctx, state, &session.Error{Code: code, Message: message, Phase: new("recovery"), Details: []session.Detail{}}); stateErr != nil {
		return stateErr
	}
	e.settleRetryAt = time.Now().Add(e.settleRetryDelay)
	e.settleRetryDelay = min(60*time.Second, e.settleRetryDelay*2)
	e.Disconnect()
	return nil
}
func (e *Executor) settleEnvironmentNow(ctx context.Context, record store.SessionRecord) error {
	if record.AllowMultipleRuns {
		return e.pauseNow(ctx, record)
	}
	return e.deleteNow(ctx, record)
}

func (e *Executor) pauseNow(ctx context.Context, record store.SessionRecord) error {
	if record.SandboxID == nil {
		return e.releaseEnvironment(ctx, record, "paused")
	}
	if record.SandboxState != "pausing" {
		if err := e.state(ctx, "pausing", nil); err != nil {
			return err
		}
	}
	if e.sandbox == nil {
		state, err := e.Platform.Info(ctx, *record.SandboxID)
		if err != nil {
			return err
		}
		if state == "paused" {
			return e.releaseEnvironment(ctx, record, "paused")
		}
		box, err := e.Platform.Connect(ctx, *record.SandboxID, 0)
		if err != nil {
			return err
		}
		e.sandbox = box
	}
	last, err := e.Store.LastTerminalRun(ctx, e.ID)
	if err != nil {
		return err
	}
	if err := e.finalAuthSync(ctx, record, last); err != nil {
		return err
	}
	o, err := e.operation(ctx, "pause", &last.ID, nil, nil)
	if err != nil {
		return err
	}
	if err := e.opStatus(ctx, o.ID, "sending", nil); err != nil {
		return err
	}
	if e.unregisterLimits != nil {
		e.unregisterLimits()
		e.unregisterLimits = nil
	}
	if err := e.sandbox.Pause(ctx); err != nil {
		return err
	}
	if err := e.opStatus(ctx, o.ID, "confirmed", nil); err != nil {
		return err
	}
	e.Disconnect()
	return e.releaseEnvironment(ctx, record, "paused")
}

func (e *Executor) deleteNow(ctx context.Context, record store.SessionRecord) error {
	if record.SandboxID == nil || record.SandboxState == "deleted" {
		return e.releaseEnvironment(ctx, record, "deleted")
	}
	if record.SandboxState != "deleting" {
		if err := e.state(ctx, "deleting", nil); err != nil {
			return err
		}
	}
	last, err := e.Store.LastTerminalRun(ctx, e.ID)
	if err != nil {
		return err
	}
	o, err := e.operation(ctx, "sandbox_delete", &last.ID, nil, record.SandboxID)
	if err != nil {
		return err
	}
	if o.Status != "confirmed" {
		// A persisted sending attempt retries deletion directly, even after restart.
		// Reconnecting only to retry kill would keep extending the sandbox lease.
		var absent bool
		if o.Status == "pending" {
			err := e.finalAuthSync(ctx, record, last)
			absent = errors.Is(err, harness.ErrNotFound)
			if err != nil && !absent {
				if record.Configuration.Harness == "opencode" {
					return err
				}
				slog.WarnContext(ctx, "Final account sync preparation failed; deleting sandbox", "session_id", e.ID, "error_type", diagnostic.Describe(err))
			}
		}
		if !absent {
			if err := e.opStatus(ctx, o.ID, "sending", nil); err != nil {
				return err
			}
			e.Disconnect()
			if err := e.Platform.Delete(ctx, *record.SandboxID); err != nil && !errors.Is(err, harness.ErrNotFound) {
				return err
			}
		}
		if err := e.opStatus(ctx, o.ID, "confirmed", nil); err != nil {
			return err
		}
	}
	e.Disconnect()
	return e.releaseEnvironment(ctx, record, "deleted")
}

// Keep the existing conditional account upload. Deletion treats preparation
// errors as best effort; pausing retains its original retry behavior.
func (e *Executor) finalAuthSync(ctx context.Context, record store.SessionRecord, last store.RunRecord) error {
	if e.settleAuthSynced || record.HarnessHome == nil {
		return nil
	}
	source, err := e.Store.SessionCredentials(ctx, record)
	if err != nil {
		return err
	}
	if source.Mode != "account" {
		return nil
	}
	failed, err := e.Store.AuthFailedBefore(ctx, e.ID, last.Number, true)
	if err != nil {
		return err
	}
	if !failed {
		if e.sandbox == nil {
			state, err := e.Platform.Info(ctx, *record.SandboxID)
			if err != nil {
				return err
			}
			if state == "paused" {
				return nil
			}
			box, err := e.Platform.Connect(ctx, *record.SandboxID, 0)
			if err != nil {
				return err
			}
			e.sandbox = box
		}
		if record.Configuration.Harness == "opencode" && e.account == nil {
			processes, err := e.sandbox.Processes(ctx)
			if err != nil {
				return err
			}
			err = e.flushSavedConsole(ctx, record, last.ID, source, matching(processes, record.ProcessID, record.LaunchID))
			if err != nil {
				return err
			}
			e.settleAuthSynced = true
			return nil
		}
		if e.account == nil {
			e.account, err = e.NewAccount(ctx, e.sandbox, *record.HarnessHome, source)
			if err != nil {
				return err
			}
		}
		if err := flushAccount(ctx, e.account); err != nil {
			return err
		}
	}
	e.settleAuthSynced = true
	return nil
}

func (e *Executor) releaseEnvironment(ctx context.Context, record store.SessionRecord, settled string) error {
	return e.Store.Mutate(ctx, e.ID, true, func(tx pgx.Tx, r *store.SessionRecord) error {
		outstanding, err := openCreate(ctx, tx, e.ID)
		if err != nil {
			return err
		}
		if outstanding != nil {
			return nil
		}
		if record.SandboxID != nil && (record.SandboxState != "unavailable" || !record.AllowMultipleRuns) {
			if err := store.UpdateSandbox(ctx, tx, r, func(r *store.SessionRecord) {
				r.SandboxState = settled
				r.SandboxLastKnownState = &settled
				r.SandboxError = nil
			}); err != nil {
				return err
			}
		}
		active, err := store.ActiveRun(ctx, tx, e.ID)
		if err != nil {
			return err
		}
		if active == nil {
			r.SlotReserved = false
		}
		return nil
	})
}

func (e *Executor) consoleDriver(record store.SessionRecord, run uuid.UUID, source session.Credentials) *opencode.Driver {
	return opencode.New(e.sandbox, e.Store.Settings.RPCTimeout, e.Store.Settings.MaxToolResultBytes, e.redactor, e.ID, run, record.Configuration.Public.Sandbox.Template, source.Provider, record.Configuration.Public.Agent.Model)
}
func flushAccount(ctx context.Context, sync credentials.Sync) error {
	if f, ok := sync.(interface{ Flush(context.Context) error }); ok {
		if err := f.Flush(ctx); err != nil {
			return harness.FailureCause("credentials_unavailable", "OpenCode account credential synchronization failed.", err)
		}
	} else {
		sync.Sync(ctx, true)
	}
	return nil
}
func (e *Executor) flushSavedConsole(ctx context.Context, record store.SessionRecord, run uuid.UUID, source session.Credentials, process *harness.Process) error {
	if record.HarnessHome == nil {
		return nil
	}
	driver := e.consoleDriver(record, run, source)
	defer func() { _ = driver.Close() }()
	if process != nil {
		if err := driver.Attach(ctx, process.PID); err != nil {
			return err
		}
		if err := driver.Initialize(ctx, source, false); err != nil {
			return err
		}
	}
	sync, err := credentials.NewOpenCode(ctx, e.sandbox, *record.HarnessHome, driver, source, e.redactor)
	if err != nil {
		return err
	}
	defer func() { _ = sync.Close() }()
	return flushAccount(ctx, sync)
}

func (e *Executor) bindHarness(ctx context.Context, run *store.RunRecord, launchID *uuid.UUID) error {
	if launchID == nil || run.ExecutionStartedAt != nil {
		return nil
	}
	return e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, record *store.SessionRecord) error {
		n, err := db.New(tx).BindRunHarnessLaunch(ctx, db.BindRunHarnessLaunchParams{RunID: run.ID, SessionID: e.ID, LaunchID: *launchID})
		if err != nil || n == 0 {
			return err
		}
		current, err := store.GetRun(ctx, tx, e.ID, run.ID)
		if err != nil {
			return err
		}
		return store.PublishRun(ctx, tx, record, &current)
	})
}
