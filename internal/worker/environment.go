package worker

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skillum-ai/orpheus/internal/config"
	"github.com/skillum-ai/orpheus/internal/harness"
	"github.com/skillum-ai/orpheus/internal/session"
	"github.com/skillum-ai/orpheus/internal/store"
)

func (e *Executor) ensureSandbox(ctx context.Context, record *store.SessionRecord, run *store.RunRecord) error {
	cfg := record.Configuration
	timeout := time.Duration(cfg.Public.Limits.RunTimeoutSeconds+3*cfg.Public.Hooks.TimeoutSeconds)*time.Second + e.Store.Settings.CancelGrace + 300*time.Second
	if record.SandboxID == nil {
		if err := e.state(ctx, "provisioning", nil); err != nil {
			return err
		}
		o, err := e.operation(ctx, "sandbox_create", nil, nil, nil)
		if err != nil {
			return err
		}
		var result operationResult
		if o.Status == "sending" || o.Status == "uncertain" {
			found, err := e.Platform.Find(ctx, map[string]string{"orpheus_session_id": e.ID.String(), "orpheus_operation_id": o.ID.String()})
			if err != nil {
				return err
			}
			if len(found) != 1 {
				return harness.ErrUncertain
			}
			result.SandboxID = found[0]
			if err := e.opStatus(ctx, o.ID, "confirmed", result); err != nil {
				return err
			}
		} else {
			result, err = e.invoke(ctx, o, func() (operationResult, error) {
				box, err := e.Platform.Create(ctx, cfg.Public.Sandbox.Template, timeout, map[string]string{"orpheus_session_id": e.ID.String(), "orpheus_operation_id": o.ID.String()})
				if err != nil {
					return operationResult{}, err
				}
				e.sandbox = box
				return operationResult{SandboxID: box.ID()}, nil
			})
			if err != nil {
				return err
			}
		}
		if result.SandboxID == "" {
			return harness.ErrUncertain
		}
		record.SandboxID = &result.SandboxID
		if err := e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, r *store.SessionRecord) error {
			return store.UpdateSandbox(ctx, tx, r, func(r *store.SessionRecord) {
				r.SandboxID = record.SandboxID
			})
		}); err != nil {
			return err
		}
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
		if err := e.opStatus(ctx, o.ID, "sending", nil); err != nil {
			return err
		}
		box, err := e.Platform.Connect(ctx, *record.SandboxID, timeout)
		if err != nil {
			return err
		}
		e.sandbox = box
		if err := e.opStatus(ctx, o.ID, "confirmed", nil); err != nil {
			return err
		}
	case e.timeoutRunID != run.ID || !time.Now().Before(e.timeoutRenewAt):
		if err := e.sandbox.SetTimeout(ctx, timeout); err != nil {
			return err
		}
	default:
		return nil
	}
	e.timeoutRunID = run.ID
	e.timeoutRenewAt = time.Now().Add(min(60*time.Second, timeout/2))
	e.pauseAuthSynced = false
	e.pauseRetryAt = time.Time{}
	e.pauseRetryDelay = 5 * time.Second
	return e.state(ctx, "ready", nil)
}
func (e *Executor) preparePaths(ctx context.Context, record *store.SessionRecord) error {
	if record.Workspace != nil && record.HarnessHome != nil {
		return nil
	}
	// Resolve the selected sandbox user's home. Python is needed only by
	// native_reader.py, not for preparing directories or selecting a user.
	output, err := e.sandbox.Run(ctx, `set -eu; h="$HOME"; w="$h/workspace"; c="$h/.orpheus-codex"; mkdir -p "$w" "$c"; chmod 700 "$c"; printf '%s\000%s\000' "$w" "$c"`)
	if err != nil {
		return err
	}
	paths := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
	if len(paths) != 2 || !strings.HasPrefix(paths[0], "/") || !strings.HasPrefix(paths[1], "/") {
		return harness.Failure("environment_unavailable", "Sandbox paths are unavailable.")
	}
	record.Workspace = &paths[0]
	record.HarnessHome = &paths[1]
	return e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, r *store.SessionRecord) error {
		return store.UpdateSandbox(ctx, tx, r, func(r *store.SessionRecord) {
			r.Workspace = record.Workspace
			r.HarnessHome = record.HarnessHome
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
	repair := false
	if cfg.Credentials.Mode == "account" && run.ExecutionStartedAt == nil {
		var err error
		repair, err = e.Store.AuthFailedBefore(ctx, e.ID, run.Number, false)
		if err != nil {
			return err
		}
	}
	if e.driver != nil && !repair {
		return nil
	}
	if e.driver != nil {
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
	if repair && process != nil {
		return e.sandbox.Kill(ctx, process.PID)
	}
	driver := e.NewDriver(e.sandbox)
	adopted := false
	defer func() {
		if !adopted {
			_ = driver.Close()
		}
	}()
	if e.account == nil && cfg.Credentials.Mode == "account" {
		e.account, err = e.NewAccount(ctx, e.sandbox, *record.HarnessHome, cfg.Credentials)
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
			if err := driver.Attach(ctx, pid); err != nil {
				return err
			}
			if err := e.opStatus(ctx, o.ID, "confirmed", operationResult{PID: pid}); err != nil {
				return err
			}
		} else {
			env, err := config.Environment(e.Store.Cipher, e.ID, record.EnvCiphertext, cfg.Public, e.Store.Settings.HarnessEnvAllowlist)
			if err != nil {
				return harness.Failure("environment_unavailable", "Harness environment is unavailable.")
			}
			if e.account != nil && (record.ProcessID == nil || repair) {
				if err := e.account.Seed(ctx); err != nil {
					return err
				}
			}
			nativeEnv, err := driver.Prepare(ctx, *record.HarnessHome, cfg.Credentials)
			if err != nil {
				return err
			}
			maps.Copy(env, nativeEnv)
			if proxy := e.Store.Settings.SandboxProxyURL; proxy != "" {
				env["ALL_PROXY"] = proxy
				env["NO_PROXY"] = "localhost,127.0.0.1"
			}
			env["ORPHEUS_LAUNCH_ID"] = o.ID.String()
			result, err := e.invoke(ctx, o, func() (operationResult, error) {
				pid, err := driver.Launch(ctx, env, *record.Workspace)
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
	if err := driver.Initialize(ctx, cfg.Credentials, !initialized); err != nil {
		return err
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
	e.driver = driver
	adopted = true
	return nil
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
		current, err := store.GetRun(ctx, tx, e.ID, run.ID)
		if err != nil {
			return err
		}
		if current.Status.Terminal() {
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
func (e *Executor) pause(ctx context.Context, record store.SessionRecord) error {
	if time.Now().Before(e.pauseRetryAt) {
		return nil
	}
	err := e.pauseNow(ctx, record)
	if err == nil {
		e.pauseRetryAt = time.Time{}
		e.pauseRetryDelay = 5 * time.Second
		return nil
	}
	if errors.Is(err, harness.ErrNotFound) {
		return err
	}
	if stateErr := e.state(ctx, "pausing", &session.Error{Code: "environment_unavailable", Message: "Sandbox pause failed; retrying.", Phase: new("recovery"), Details: []session.Detail{}}); stateErr != nil {
		return stateErr
	}
	e.pauseRetryAt = time.Now().Add(e.pauseRetryDelay)
	e.pauseRetryDelay = min(60*time.Second, e.pauseRetryDelay*2)
	e.Disconnect()
	return nil
}
func (e *Executor) pauseNow(ctx context.Context, record store.SessionRecord) error {
	if record.SandboxID != nil {
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
			if state != "paused" {
				box, err := e.Platform.Connect(ctx, *record.SandboxID, 0)
				if err != nil {
					return err
				}
				e.sandbox = box
			}
		}
		if e.sandbox != nil {
			last, err := e.Store.LastTerminalRun(ctx, e.ID)
			if err != nil {
				return err
			}
			if !e.pauseAuthSynced {
				failed, err := e.Store.AuthFailedBefore(ctx, e.ID, last.Number, true)
				if err != nil {
					return err
				}
				if !failed {
					if e.account == nil && record.HarnessHome != nil && record.Configuration.Credentials.Mode == "account" {
						e.account, err = e.NewAccount(ctx, e.sandbox, *record.HarnessHome, record.Configuration.Credentials)
						if err != nil {
							return err
						}
					}
					if e.account != nil {
						e.account.Sync(ctx, true)
					}
				}
				e.pauseAuthSynced = true
			}
			o, err := e.operation(ctx, "pause", &last.ID, nil, nil)
			if err != nil {
				return err
			}
			if err := e.opStatus(ctx, o.ID, "sending", nil); err != nil {
				return err
			}
			if err := e.sandbox.Pause(ctx); err != nil {
				return err
			}
			if err := e.opStatus(ctx, o.ID, "confirmed", nil); err != nil {
				return err
			}
			e.Disconnect()
		}
		if record.SandboxState != "unavailable" {
			if err := e.state(ctx, "paused", nil); err != nil {
				return err
			}
		}
	}
	return e.Store.Mutate(ctx, e.ID, true, func(tx pgx.Tx, r *store.SessionRecord) error {
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
