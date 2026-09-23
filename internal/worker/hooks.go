package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skillum-ai/orpheus/internal/config"
	"github.com/skillum-ai/orpheus/internal/harness"
	"github.com/skillum-ai/orpheus/internal/session"
	"github.com/skillum-ai/orpheus/internal/store"
)

type hookStarted struct {
	OperationID string `json:"operation_id"`
	WrapperPID  int    `json:"wrapper_pid"`
	HookPID     *int   `json:"hook_pid"`
}

type hookResultFile struct {
	OperationID        string    `json:"operation_id"`
	FinishedAt         time.Time `json:"finished_at"`
	ExitCode           *int      `json:"exit_code"`
	Signal             *int      `json:"signal"`
	OutputCompleteness string    `json:"output_completeness"`
	OriginalBytes      int64     `json:"original_bytes"`
	HeadFile           string    `json:"head_file"`
	TailFile           string    `json:"tail_file"`
}

func hookScript(cfg session.HooksConfiguration, name string) *string {
	switch name {
	case "after_create":
		return cfg.AfterCreate
	case "before_run":
		return cfg.BeforeRun
	case "after_run":
		return cfg.AfterRun
	case "before_remove":
		return cfg.BeforeRemove
	default:
		return nil
	}
}

func hookDir(record store.SessionRecord, id uuid.UUID) string {
	return path.Join(path.Dir(*record.Workspace), ".orpheus", "hooks", id.String())
}

func (e *Executor) ensureRunner(ctx context.Context, record store.SessionRecord) (string, error) {
	home := path.Dir(*record.Workspace)
	target := path.Join(home, ".orpheus", "bin", "hook-runner")
	if e.runnerReady {
		return target, nil
	}
	raw, err := e.sandbox.Run(ctx, "uname -m")
	if err != nil {
		return "", err
	}
	arch := strings.TrimSpace(string(raw))
	switch arch {
	case "x86_64":
		arch = "amd64"
	case "aarch64":
		arch = "arm64"
	default:
		return "", harness.Failure("hook_environment_unavailable", "Sandbox architecture is not supported for hooks.")
	}
	if _, err := e.sandbox.Run(ctx, "mkdir -p "+harness.Quote(path.Dir(target))+" && chmod 700 "+harness.Quote(path.Dir(target))); err != nil {
		return "", err
	}
	binary, err := e.RunnerBinary("/opt/orpheus/hook-runner-" + arch)
	if err != nil {
		return "", err
	}
	if err := e.sandbox.Write(ctx, target, binary); err != nil {
		return "", err
	}
	if _, err := e.sandbox.Run(ctx, "chmod 700 "+harness.Quote(target)); err != nil {
		return "", err
	}
	e.runnerReady = true
	return target, nil
}

func (e *Executor) hookEnvironment(record store.SessionRecord, run store.RunRecord, name string) (map[string]string, error) {
	var runEnv map[string]string
	var runEnvFrom []string
	if name == "before_run" || name == "after_run" {
		var err error
		runEnv, err = e.Store.Cipher.DecryptRun(e.ID, run.ID, run.EnvCiphertext)
		if err != nil {
			return nil, err
		}
		runEnvFrom = run.EnvFrom
	}
	env, err := config.MergedEnvironment(e.Store.Cipher, e.ID, record.EnvCiphertext, record.Configuration.Public, e.Store.Settings.HarnessEnvAllowlist, runEnv, runEnvFrom)
	if err != nil {
		return nil, err
	}
	if name == "before_run" || name == "after_run" {
		env["ORPHEUS_RUN_ID"] = run.ID.String()
		if run.InputFingerprint != nil {
			env["ORPHEUS_INPUT_FINGERPRINT"] = *run.InputFingerprint
		}
	}
	env["ORPHEUS_SESSION_ID"] = e.ID.String()
	env["ORPHEUS_WORKSPACE_PATH"] = *record.Workspace
	if name == "after_run" {
		if run.AgentStatus != nil {
			env["ORPHEUS_AGENT_STATUS"] = string(*run.AgentStatus)
		}
		if run.StopReason != nil {
			env["ORPHEUS_STOP_REASON"] = *run.StopReason
		}
	}
	return env, nil
}

func hookProblem(name, code, message string) *session.Error {
	phase := "preparation"
	if name == "after_run" {
		phase = "finalization"
	}
	return &session.Error{Code: code, Message: message, Phase: &phase, Details: []session.Detail{{Path: []any{"configuration", "hooks", name}, Code: "invalid_value"}}}
}

func (e *Executor) failHook(ctx context.Context, run store.RunRecord, name string, problem *session.Error) error {
	return e.Store.ChangeHook(ctx, e.ID, run.ID, name, func(h *store.HookExecution, _ *store.RunRecord) error {
		if h.Status == "failed" || h.Status == "completed" || h.Status == "cancelled" {
			return nil
		}
		h.Status = "failed"
		h.Error = problem
		h.OutputCompleteness = "unavailable"
		h.FinishedAt = new(time.Now().UTC())
		return nil
	})
}

func (e *Executor) startHook(ctx context.Context, record store.SessionRecord, run store.RunRecord, h store.HookExecution, name string) error {
	runner, err := e.ensureRunner(ctx, record)
	if err != nil {
		if failure, ok := errors.AsType[*harness.ExecutionError](err); ok {
			return e.failHook(ctx, run, name, hookProblem(name, failure.Code, failure.Message))
		}
		return err
	}
	dir := hookDir(record, h.ID)
	if _, err := e.sandbox.Run(ctx, "mkdir -p "+harness.Quote(dir)+" && chmod 700 "+harness.Quote(dir)); err != nil {
		return err
	}
	script := path.Join(dir, "script")
	if err := e.sandbox.Write(ctx, script, []byte(*hookScript(record.Configuration.Public.Hooks, name))); err != nil {
		return err
	}
	if _, err := e.sandbox.Run(ctx, "chmod 700 "+harness.Quote(script)); err != nil {
		return err
	}
	if err := e.Store.ChangeHook(ctx, e.ID, run.ID, name, func(h *store.HookExecution, r *store.RunRecord) error {
		if r.CancelRequestedAt != nil && name != "after_run" {
			return nil
		}
		if h.Status != "pending" {
			return nil
		}
		now := time.Now().UTC()
		h.Status = "running"
		h.StartedAt = &now
		h.DeadlineAt = new(now.Add(time.Duration(record.Configuration.Public.Hooks.TimeoutSeconds) * time.Second))
		r.Phase = &name
		return nil
	}); err != nil {
		return err
	}
	current, err := e.Store.Hook(ctx, run.ID, name)
	if err != nil || current == nil || current.Status != "running" {
		return err
	}
	return e.launchHook(ctx, record, run, h, name, runner)
}

func (e *Executor) launchHook(ctx context.Context, record store.SessionRecord, run store.RunRecord, h store.HookExecution, name, runner string) error {
	env, err := e.hookEnvironment(record, run, name)
	if err != nil {
		return e.failHook(ctx, run, name, hookProblem(name, "hook_environment_unavailable", "Hook environment is unavailable."))
	}
	env["ORPHEUS_HOOK_OPERATION_ID"] = h.ID.String()
	dir := hookDir(record, h.ID)
	script := path.Join(dir, "script")
	command := "exec " + harness.Quote(runner) + " -operation-id " + harness.Quote(h.ID.String()) +
		" -operation-dir " + harness.Quote(dir) + " -script " + harness.Quote(script) +
		" -workspace " + harness.Quote(*record.Workspace) + " -max-output-bytes " + strconv.Itoa(e.Store.Settings.MaxHookOutputBytes)
	stream, _, err := e.sandbox.Start(ctx, command, env, *record.Workspace)
	if stream != nil {
		_ = stream.Close()
	}
	if errors.Is(err, harness.ErrRejected) || errors.Is(err, harness.ErrNotFound) {
		return e.failHook(ctx, run, name, hookProblem(name, "hook_launch_failed", "Hook process could not start."))
	}
	return err
}

func (e *Executor) sandboxFile(ctx context.Context, name string) ([]byte, bool, error) {
	r, err := e.sandbox.Read(ctx, name)
	if errors.Is(err, harness.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(io.LimitReader(r, int64(e.Store.Settings.MaxHookOutputBytes)+4096))
	return b, true, err
}

func (e *Executor) finishHookFromFile(ctx context.Context, record store.SessionRecord, run store.RunRecord, h store.HookExecution, name string, raw []byte) error {
	var result hookResultFile
	if json.Unmarshal(raw, &result) != nil || result.OperationID != h.ID.String() || result.FinishedAt.IsZero() || result.HeadFile != "head.txt" || (result.TailFile != "" && result.TailFile != "tail.txt") {
		return e.failHook(ctx, run, name, hookProblem(name, "hook_result_unavailable", "Hook result is invalid."))
	}
	if result.OutputCompleteness != "complete" && result.OutputCompleteness != "truncated" {
		return e.failHook(ctx, run, name, hookProblem(name, "hook_result_unavailable", "Hook output metadata is invalid."))
	}
	dir := hookDir(record, h.ID)
	head, ok, err := e.sandboxFile(ctx, path.Join(dir, result.HeadFile))
	if err != nil {
		return err
	}
	if !ok {
		return e.failHook(ctx, run, name, hookProblem(name, "hook_result_unavailable", "Hook output is unavailable."))
	}
	var tail []byte
	if result.TailFile != "" {
		tail, ok, err = e.sandboxFile(ctx, path.Join(dir, result.TailFile))
		if err != nil {
			return err
		}
		if !ok {
			return e.failHook(ctx, run, name, hookProblem(name, "hook_result_unavailable", "Hook output is unavailable."))
		}
	}
	if len(head)+len(tail) > e.Store.Settings.MaxHookOutputBytes+8 {
		return e.failHook(ctx, run, name, hookProblem(name, "hook_result_unavailable", "Hook output exceeds the configured limit."))
	}
	var output any
	if result.OutputCompleteness == "truncated" {
		output = struct {
			Type          string `json:"type"`
			SourceType    string `json:"source_type"`
			Head          string `json:"head"`
			Tail          string `json:"tail"`
			OriginalBytes int64  `json:"original_bytes"`
			ExitCode      *int   `json:"exit_code"`
		}{"truncated_text", "text", string(head), string(tail), result.OriginalBytes, result.ExitCode}
	} else {
		output = map[string]any{"type": "text", "text": string(head), "original_bytes": result.OriginalBytes, "exit_code": result.ExitCode}
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return err
	}
	return e.Store.ChangeHook(ctx, e.ID, run.ID, name, func(h *store.HookExecution, _ *store.RunRecord) error {
		if h.Status != "running" {
			return nil
		}
		h.FinishedAt, h.ExitCode, h.Signal = new(time.Now().UTC()), result.ExitCode, result.Signal
		h.Output = encoded
		h.OutputCompleteness = result.OutputCompleteness
		if result.OutputCompleteness == "truncated" {
			h.TruncationReason = new("orpheus_limit")
		}
		switch {
		case h.StopReason != nil && *h.StopReason == "timeout":
			h.Status = "failed"
			h.Error = hookProblem(name, "hook_timeout", "Hook exceeded its deadline.")
		case h.StopReason != nil && *h.StopReason == "cancelled":
			h.Status = "cancelled"
		case result.ExitCode != nil && *result.ExitCode == 0:
			h.Status = "completed"
		case result.ExitCode == nil && result.Signal == nil:
			h.Status = "failed"
			h.Error = hookProblem(name, "hook_launch_failed", "Hook process could not start.")
		default:
			h.Status = "failed"
			h.Error = hookProblem(name, "hook_failed", "Hook returned an error.")
		}
		return nil
	})
}

func (e *Executor) finishStoppedHook(ctx context.Context, run store.RunRecord, name string, h store.HookExecution) error {
	return e.Store.ChangeHook(ctx, e.ID, run.ID, name, func(current *store.HookExecution, _ *store.RunRecord) error {
		if current.Status != "running" {
			return nil
		}
		current.FinishedAt = new(time.Now().UTC())
		current.OutputCompleteness = "unavailable"
		if h.StopReason != nil && *h.StopReason == "cancelled" {
			current.Status = "cancelled"
		} else {
			current.Status = "failed"
			current.Error = hookProblem(name, "hook_timeout", "Hook exceeded its deadline.")
		}
		return nil
	})
}

func (e *Executor) observeHook(ctx context.Context, record store.SessionRecord, run store.RunRecord, h store.HookExecution, name string) error {
	dir := hookDir(record, h.ID)
	raw, startedFound, err := e.sandboxFile(ctx, path.Join(dir, "started.json"))
	if err != nil {
		return err
	}
	var started hookStarted
	if startedFound && (json.Unmarshal(raw, &started) != nil || started.OperationID != h.ID.String() || started.WrapperPID <= 0 || (started.HookPID != nil && *started.HookPID <= 0)) {
		return e.failHook(ctx, run, name, hookProblem(name, "hook_result_unavailable", "Hook start record is invalid."))
	}
	processes, err := e.sandbox.Processes(ctx)
	if err != nil {
		return err
	}
	var wrapperPID int
	for _, p := range processes {
		if p.Env["ORPHEUS_HOOK_OPERATION_ID"] == h.ID.String() {
			wrapperPID = p.PID
			break
		}
	}
	// Read result after the process list: a runner that exits during the list
	// has already published its result by the time this read completes.
	raw, resultFound, err := e.sandboxFile(ctx, path.Join(dir, "result.json"))
	if err != nil {
		return err
	}
	if resultFound {
		return e.finishHookFromFile(ctx, record, run, h, name, raw)
	}
	stopping := run.CancelRequestedAt != nil && name != "after_run"
	timeout := h.DeadlineAt != nil && !time.Now().Before(*h.DeadlineAt)
	if startedFound && wrapperPID == 0 {
		if h.StopReason != nil {
			return e.finishStoppedHook(ctx, run, name, h)
		}
		return e.failHook(ctx, run, name, hookProblem(name, "hook_result_unavailable", "Hook process ended without a result."))
	}
	if !startedFound && wrapperPID == 0 && !stopping && !timeout {
		_, claimed, err := e.sandboxFile(ctx, path.Join(dir, "claim"))
		if err != nil {
			return err
		}
		if !claimed {
			// An uncertain Start or worker crash before Start can be retried with
			// the same ID. The runner's exclusive claim prevents a second script.
			runner, err := e.ensureRunner(ctx, record)
			if err != nil {
				return err
			}
			return e.launchHook(ctx, record, run, h, name, runner)
		}
		if h.StartedAt != nil && time.Since(*h.StartedAt) > 5*time.Second {
			return e.failHook(ctx, run, name, hookProblem(name, "hook_result_unavailable", "Hook process ended before recording its start."))
		}
	}
	if !stopping && !timeout {
		return nil
	}
	if h.CancelAttemptedAt == nil {
		reason := "timeout"
		attempted := time.Now().UTC()
		if err := e.Store.ChangeHook(ctx, e.ID, run.ID, name, func(h *store.HookExecution, r *store.RunRecord) error {
			if h.Status != "running" {
				return nil
			}
			if r.CancelRequestedAt != nil && name != "after_run" {
				reason = "cancelled"
			}
			h.CancelAttemptedAt = &attempted
			h.StopReason = &reason
			return nil
		}); err != nil {
			return err
		}
		current, err := e.Store.Hook(ctx, run.ID, name)
		if err != nil || current == nil || current.Status != "running" {
			return err
		}
		h = *current
	}
	if time.Now().Before(h.CancelAttemptedAt.Add(e.Store.Settings.CancelGrace)) {
		if started.HookPID != nil && e.hookTermSent != h.ID {
			if _, err := e.sandbox.Run(ctx, fmt.Sprintf("kill -TERM %d 2>/dev/null || true", *started.HookPID)); err != nil {
				return err
			}
			e.hookTermSent = h.ID
		}
		return nil
	}
	if started.HookPID != nil {
		if _, err := e.sandbox.Run(ctx, fmt.Sprintf("kill -KILL %d 2>/dev/null || true", *started.HookPID)); err != nil {
			return err
		}
	}
	if wrapperPID != 0 {
		if err := e.sandbox.Kill(ctx, wrapperPID); err != nil && !errors.Is(err, harness.ErrNotFound) {
			return err
		}
	}
	return e.finishStoppedHook(ctx, run, name, h)
}

// runHook performs at most one external transition per tick. A finished hook is
// never restarted; the sandbox result file is the recovery point after Start.
func (e *Executor) runHook(ctx context.Context, record store.SessionRecord, run store.RunRecord, name string) (bool, *session.Error, error) {
	h, err := e.Store.Hook(ctx, run.ID, name)
	if err != nil {
		return false, nil, err
	}
	if h == nil {
		return true, nil, nil
	}
	switch h.Status {
	case "completed":
		e.cleanupHook(ctx, record, *h)
		return true, nil, nil
	case "failed", "cancelled", "skipped":
		e.cleanupHook(ctx, record, *h)
		return true, h.Error, nil
	case "pending":
		if err := e.startHook(ctx, record, run, *h, name); err != nil {
			return false, nil, err
		}
	case "running":
		if err := e.observeHook(ctx, record, run, *h, name); err != nil {
			return false, nil, err
		}
	}
	return false, nil, nil
}

func (e *Executor) cleanupHook(ctx context.Context, record store.SessionRecord, h store.HookExecution) {
	if len(h.Output) == 0 || e.sandbox == nil || record.Workspace == nil {
		return
	}
	// The database is now authoritative; retain no completed output in the
	// sandbox longer than needed for recovery. Cleanup is best effort.
	_, _ = e.sandbox.Run(ctx, "rm -rf -- "+harness.Quote(hookDir(record, h.ID)))
}

func (e *Executor) skipHooks(ctx context.Context, run store.RunRecord, names ...string) error {
	for _, name := range names {
		h, err := e.Store.Hook(ctx, run.ID, name)
		if err != nil {
			return err
		}
		if h == nil || h.Status != "pending" {
			continue
		}
		if err := e.Store.ChangeHook(ctx, e.ID, run.ID, name, func(h *store.HookExecution, _ *store.RunRecord) error {
			h.Status = "skipped"
			h.FinishedAt = new(time.Now().UTC())
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (e *Executor) finishPreparationError(ctx context.Context, run store.RunRecord, name string, problem *session.Error) error {
	if problem == nil {
		problem = hookProblem(name, "hook_failed", "Hook failed.")
	}
	if err := e.skipHooks(ctx, run, "before_run", "after_run"); err != nil {
		return err
	}
	return e.Store.Mutate(ctx, e.ID, false, func(tx pgx.Tx, r *store.SessionRecord) error {
		current, err := store.GetRun(ctx, tx, e.ID, run.ID)
		if err != nil {
			return err
		}
		return store.Finish(ctx, tx, r, &current, session.Failed, problem, nil)
	})
}
