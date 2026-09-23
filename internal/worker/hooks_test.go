//go:build integration

package worker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skillum-ai/orpheus/internal/config"
	"github.com/skillum-ai/orpheus/internal/harness"
	"github.com/skillum-ai/orpheus/internal/secret"
	"github.com/skillum-ai/orpheus/internal/session"
	"github.com/skillum-ai/orpheus/internal/store"
	"github.com/skillum-ai/orpheus/internal/testutil"
)

type hookStream struct{}

func (hookStream) Stdout() <-chan []byte                      { return nil }
func (hookStream) Stderr() <-chan []byte                      { return nil }
func (hookStream) Write(context.Context, []byte) (int, error) { return 0, nil }
func (hookStream) Close() error                               { return nil }

type hookRemote struct {
	*remote
	files           map[string][]byte
	invoked         []string
	hookEnv         []map[string]string
	signals         []string
	autoResult      bool
	failAfter       bool
	failCreate      bool
	lostHookStart   bool
	alwaysLostStart bool
	startAttempts   int
	cleanups        int
	onProcesses     func()
}

func (h *hookRemote) Create(ctx context.Context, template string, timeout time.Duration, metadata map[string]string) (harness.Sandbox, error) {
	_, err := h.remote.Create(ctx, template, timeout, metadata)
	return h, err
}
func (h *hookRemote) Connect(ctx context.Context, id string, timeout time.Duration) (harness.Sandbox, error) {
	_, err := h.remote.Connect(ctx, id, timeout)
	return h, err
}
func (h *hookRemote) Run(ctx context.Context, command string) ([]byte, error) {
	if command == "uname -m" {
		return []byte("x86_64\n"), nil
	}
	if strings.HasPrefix(command, "mkdir ") || strings.HasPrefix(command, "chmod ") || strings.HasPrefix(command, "kill ") {
		if strings.HasPrefix(command, "kill ") {
			h.signals = append(h.signals, command)
		}
		return nil, nil
	}
	if strings.HasPrefix(command, "rm -rf -- ") {
		h.cleanups++
		return nil, nil
	}
	return h.remote.Run(ctx, command)
}
func (h *hookRemote) Write(_ context.Context, name string, data []byte) error {
	h.files[name] = append([]byte(nil), data...)
	return nil
}
func (h *hookRemote) Read(_ context.Context, name string) (io.ReadCloser, error) {
	data, ok := h.files[name]
	if !ok {
		return nil, harness.ErrNotFound
	}
	return io.NopCloser(strings.NewReader(string(data))), nil
}
func (h *hookRemote) Processes(ctx context.Context) ([]harness.Process, error) {
	processes, err := h.remote.Processes(ctx)
	if h.onProcesses != nil {
		callback := h.onProcesses
		h.onProcesses = nil
		callback()
	}
	return processes, err
}
func (h *hookRemote) Start(_ context.Context, _ string, env map[string]string, _ string) (harness.Stream, int, error) {
	h.startAttempts++
	if h.alwaysLostStart {
		return nil, 0, harness.ErrUncertain
	}
	if h.lostHookStart {
		h.lostHookStart = false
		return nil, 0, harness.ErrUncertain
	}
	id := env["ORPHEUS_HOOK_OPERATION_ID"]
	dir := path.Join("/home/template/.orpheus/hooks", id)
	name := strings.TrimSpace(string(h.files[path.Join(dir, "script")]))
	h.invoked = append(h.invoked, name)
	h.hookEnv = append(h.hookEnv, maps.Clone(env))
	if h.autoResult {
		h.completeHook(id, name)
	}
	return hookStream{}, 200 + len(h.invoked), nil
}
func (h *hookRemote) completeHook(id, name string) {
	dir := path.Join("/home/template/.orpheus/hooks", id)
	code := 0
	if h.failAfter && strings.Contains(name, "after_run") {
		code = 7
	}
	if h.failCreate && strings.Contains(name, "after_create") {
		code = 7
	}
	h.files[path.Join(dir, "head.txt")] = []byte(name)
	data, _ := json.Marshal(hookResultFile{OperationID: id, FinishedAt: time.Now().UTC(), ExitCode: &code, OutputCompleteness: "complete", OriginalBytes: int64(len(name)), HeadFile: "head.txt"})
	h.files[path.Join(dir, "result.json")] = data
}

func setupHooks(t *testing.T, autoResult, failAfter bool, initialRunEnv ...map[string]string) (*store.Store, *hookRemote, session.Acceptance, *Executor) {
	t.Helper()
	pool := testutil.Database(t)
	cipher, _ := secret.New(base64.URLEncoding.EncodeToString(make([]byte, 32)))
	settings := config.DefaultSettings()
	settings.DatabaseURL = pool.Config().ConnString()
	s := &store.Store{Pool: pool, Settings: settings, Cipher: cipher, Profiles: config.Profiles{Profiles: map[string]config.Profile{"p": {Harness: "codex", Model: new("model"), Auth: config.Auth{Mode: "api_key", APIKeyEnv: "KEY"}}}}}
	hooks := &session.HooksInput{AfterCreate: new("#!/bin/sh\n# after_create\n"), BeforeRun: new("#!/bin/sh\n# before_run\n"), AfterRun: new("#!/bin/sh\n# after_run\n")}
	sandbox := session.SandboxInput{Template: "codex"}
	var runEnv map[string]string
	if len(initialRunEnv) > 0 {
		runEnv = initialRunEnv[0]
		sandbox.Env = map[string]string{"TOKEN": "session-value"}
	}
	a, err := s.Accept(t.Context(), store.Admission{Key: uuid.New(), Create: &session.CreateSession{Configuration: session.ConfigurationInput{Agent: session.AgentInput{Profile: "p"}, Sandbox: sandbox, Limits: session.Limits{RunTimeoutSeconds: 3600}, Hooks: hooks}, Message: session.TextMessage{Text: "task"}, Env: runEnv}})
	if err != nil {
		t.Fatal(err)
	}
	box := &hookRemote{remote: &remote{}, files: map[string][]byte{}, autoResult: autoResult, failAfter: failAfter}
	e := executor(a.SessionID, s, box.remote)
	e.Platform = box
	e.RunnerBinary = func(string) ([]byte, error) { return []byte("runner"), nil }
	t.Cleanup(e.Disconnect)
	return s, box, a, e
}

func tickUntil(t *testing.T, e *Executor, ready func() bool) {
	t.Helper()
	for range 15 {
		if ready() {
			return
		}
		tick(t, e)
	}
	t.Fatal("worker did not reach expected state")
}

func TestHooksFullCycleAndFinalMessage(t *testing.T) {
	s, box, a, e := setupHooks(t, true, false)
	tickUntil(t, e, func() bool { return box.starts == 1 })
	if len(box.invoked) != 2 || !strings.Contains(box.invoked[0], "after_create") || !strings.Contains(box.invoked[1], "before_run") {
		t.Fatalf("wrong preparation sequence: %q", box.invoked)
	}
	if box.cleanups != 2 {
		t.Fatalf("completed preparation hooks were not cleaned once: %d", box.cleanups)
	}
	for range 3 {
		tick(t, e)
	}
	if box.cleanups != 2 {
		t.Fatalf("completed hooks were cleaned again: %d", box.cleanups)
	}
	complete(box.remote)
	tickUntil(t, e, func() bool {
		run, err := s.Run(t.Context(), a.SessionID, a.RunID)
		return err == nil && run.Status == session.Finalizing
	})
	run, err := s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || run.FinalMessage == nil || run.FinalMessage.Text != "DONE" || run.FinishedAt != nil || run.AgentStatus == nil || *run.AgentStatus != session.Completed {
		t.Fatal(run, err)
	}
	view, err := s.Session(t.Context(), a.SessionID)
	if err != nil || view.Status != session.Finalizing || view.Phase == nil || *view.Phase != "after_run" || view.FinalMessage == nil {
		t.Fatal(view, err)
	}
	if _, err := s.Cancel(t.Context(), a.SessionID, a.RunID); err == nil || err.Error() != "run_not_cancellable" {
		t.Fatalf("finalizing run accepted cancellation: %v", err)
	}
	if _, err := s.Accept(t.Context(), store.Admission{SessionID: a.SessionID, Key: uuid.New(), Text: "too early"}); err == nil {
		t.Fatal("accepted another run before after_run completed")
	}
	tickUntil(t, e, func() bool {
		run, err := s.Run(t.Context(), a.SessionID, a.RunID)
		return err == nil && run.Status == session.Completed
	})
	run, err = s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || len(run.Hooks) != 3 || len(box.invoked) != 3 || run.Hooks[2].Status != "completed" {
		t.Fatal(run, box.invoked, err)
	}
	for _, list := range []func() (session.Page[session.Run], error){
		func() (session.Page[session.Run], error) {
			return s.ListRuns(t.Context(), a.SessionID, 50, "", store.ListFilter{})
		},
		func() (session.Page[session.Run], error) {
			return s.ListAllRuns(t.Context(), 50, "", store.ListFilter{})
		},
	} {
		page, err := list()
		if err != nil || len(page.Items) != 1 || len(page.Items[0].Hooks) != 3 || page.Items[0].FinalMessage == nil || page.Items[0].FinalMessage.Text != "DONE" {
			t.Fatal(page, err)
		}
	}
	if _, err := s.Accept(t.Context(), store.Admission{SessionID: a.SessionID, Key: uuid.New(), Text: "again"}); err != nil {
		t.Fatal(err)
	}
	tickUntil(t, e, func() bool { return box.starts == 2 })
	if len(box.invoked) != 4 || !strings.Contains(box.invoked[3], "before_run") {
		t.Fatalf("after_create repeated or before_run missing: %q", box.invoked)
	}
}

func TestRunEnvironmentReachesOnlyRunHooks(t *testing.T) {
	_, box, _, e := setupHooks(t, true, false, map[string]string{"TOKEN": "run-value", "TASK_ID": "42"})
	tickUntil(t, e, func() bool { return box.starts == 1 })
	if len(box.hookEnv) != 2 || box.hookEnv[0]["TOKEN"] != "session-value" || box.hookEnv[1]["TOKEN"] != "run-value" || box.hookEnv[1]["TASK_ID"] != "42" || box.env["TOKEN"] != "session-value" {
		t.Fatal(box.hookEnv, box.env)
	}
	if _, present := box.hookEnv[0]["TASK_ID"]; present {
		t.Fatal("run environment reached after_create")
	}
	if _, present := box.env["TASK_ID"]; present {
		t.Fatal("run environment reached harness")
	}
	complete(box.remote)
	tickUntil(t, e, func() bool { return len(box.hookEnv) == 3 })
	if box.hookEnv[2]["TOKEN"] != "run-value" || box.hookEnv[2]["TASK_ID"] != "42" || box.hookEnv[2]["ORPHEUS_AGENT_STATUS"] != "completed" {
		t.Fatal(box.hookEnv[2])
	}
}

func TestAfterRunFailurePreservesAgentResult(t *testing.T) {
	s, box, a, e := setupHooks(t, true, true)
	tickUntil(t, e, func() bool { return box.starts == 1 })
	complete(box.remote)
	tickUntil(t, e, func() bool {
		run, err := s.Run(t.Context(), a.SessionID, a.RunID)
		return err == nil && run.Status.Terminal()
	})
	run, err := s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || run.Status != session.Failed || run.AgentStatus == nil || *run.AgentStatus != session.Completed || run.FinalMessage == nil || run.FinalMessage.Text != "DONE" || run.Error == nil || run.Error.Code != "hook_failed" || run.Hooks[2].Error == nil {
		t.Fatal(run, err)
	}
}

func TestFailedAfterCreateRunsAgainForNextAssignment(t *testing.T) {
	s, box, a, e := setupHooks(t, true, false)
	box.failCreate = true
	tickUntil(t, e, func() bool {
		run, err := s.Run(t.Context(), a.SessionID, a.RunID)
		return err == nil && run.Status.Terminal()
	})
	first, err := s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || first.Status != session.Failed || first.AgentStatus != nil || first.Hooks[0].Status != "failed" || first.Hooks[2].Status != "skipped" || box.starts != 0 {
		t.Fatal(first, err)
	}
	box.failCreate = false
	next, err := s.Accept(t.Context(), store.Admission{SessionID: a.SessionID, Key: uuid.New(), Text: "retry"})
	if err != nil {
		t.Fatal(err)
	}
	tickUntil(t, e, func() bool { return box.starts == 1 })
	second, err := s.Run(t.Context(), a.SessionID, next.RunID)
	if err != nil || len(second.Hooks) != 3 || second.Hooks[0].Status != "completed" || len(box.invoked) != 3 {
		t.Fatal(second, box.invoked, err)
	}
}

func TestAfterRunReceivesFailedAndCancelledAgentStatus(t *testing.T) {
	for _, outcome := range []session.Status{session.Failed, session.Cancelled} {
		t.Run(string(outcome), func(t *testing.T) {
			s, box, a, e := setupHooks(t, true, false)
			tickUntil(t, e, func() bool { return box.starts == 1 })
			if outcome == session.Cancelled {
				if _, err := s.Cancel(t.Context(), a.SessionID, a.RunID); err != nil {
					t.Fatal(err)
				}
			} else {
				box.turns[0].ErrorCode = "harness_failed"
			}
			box.turns[0].Status = outcome
			tickUntil(t, e, func() bool {
				run, err := s.Run(t.Context(), a.SessionID, a.RunID)
				return err == nil && run.Status.Terminal()
			})
			run, err := s.Run(t.Context(), a.SessionID, a.RunID)
			if err != nil || run.Status != outcome || run.AgentStatus == nil || *run.AgentStatus != outcome || len(box.hookEnv) != 3 || box.hookEnv[2]["ORPHEUS_AGENT_STATUS"] != string(outcome) {
				t.Fatal(run, box.hookEnv, err)
			}
		})
	}
}

func TestExecutionErrorStillRunsAfterRun(t *testing.T) {
	for _, code := range []string{"harness_failed", "context_lost"} {
		t.Run(code, func(t *testing.T) {
			s, box, a, e := setupHooks(t, true, false)
			tickUntil(t, e, func() bool { return box.starts == 1 })
			if err := e.failure(t.Context(), &harness.ExecutionError{Code: code, Message: "Agent execution failed."}); err != nil {
				t.Fatal(err)
			}
			run, err := s.Run(t.Context(), a.SessionID, a.RunID)
			if err != nil || run.Status != session.Finalizing || run.AgentStatus == nil || *run.AgentStatus != session.Failed || run.Hooks[2].Status != "pending" {
				t.Fatal(run, err)
			}
			tickUntil(t, e, func() bool {
				run, err := s.Run(t.Context(), a.SessionID, a.RunID)
				return err == nil && run.Status.Terminal()
			})
			run, err = s.Run(t.Context(), a.SessionID, a.RunID)
			if err != nil || run.Status != session.Failed || run.Hooks[2].Status != "completed" || box.hookEnv[2]["ORPHEUS_AGENT_STATUS"] != "failed" {
				t.Fatal(run, box.hookEnv, err)
			}
		})
	}
}

func TestHookResultRecoveredWithoutRelaunch(t *testing.T) {
	s, box, a, e := setupHooks(t, false, false)
	tickUntil(t, e, func() bool { return len(box.invoked) == 1 })
	hook, err := s.Hook(t.Context(), a.RunID, "after_create")
	if err != nil || hook == nil {
		t.Fatal(hook, err)
	}
	dir := path.Join("/home/template/.orpheus/hooks", hook.ID.String())
	started, _ := json.Marshal(hookStarted{OperationID: hook.ID.String(), WrapperPID: 200})
	box.files[path.Join(dir, "started.json")] = started
	box.processes = append(box.processes, harness.Process{PID: 200, Env: map[string]string{"ORPHEUS_HOOK_OPERATION_ID": hook.ID.String()}})
	e.Disconnect()
	replacement := executor(a.SessionID, s, box.remote)
	replacement.Platform = box
	replacement.RunnerBinary = func(string) ([]byte, error) { return []byte("runner"), nil }
	t.Cleanup(replacement.Disconnect)
	tick(t, replacement)
	if len(box.invoked) != 1 {
		t.Fatal("hook relaunched after worker replacement")
	}
	hook, err = s.Hook(t.Context(), a.RunID, "after_create")
	if err != nil || hook == nil {
		t.Fatal(hook, err)
	}
	box.completeHook(hook.ID.String(), string(box.files[path.Join("/home/template/.orpheus/hooks", hook.ID.String(), "script")]))
	tickUntil(t, replacement, func() bool {
		h, err := s.Hook(t.Context(), a.RunID, "after_create")
		return err == nil && h != nil && h.Status == "completed"
	})
	if len(box.invoked) != 1 {
		t.Fatal("hook repeated while recovering result")
	}
}

func TestHookResultPublishedDuringProcessListIsNotLost(t *testing.T) {
	s, box, a, e := setupHooks(t, false, false)
	tickUntil(t, e, func() bool { return len(box.invoked) == 1 })
	h, err := s.Hook(t.Context(), a.RunID, "after_create")
	if err != nil || h == nil {
		t.Fatal(h, err)
	}
	dir := path.Join("/home/template/.orpheus/hooks", h.ID.String())
	started, _ := json.Marshal(hookStarted{OperationID: h.ID.String(), WrapperPID: 200})
	box.files[path.Join(dir, "started.json")] = started
	box.onProcesses = func() { box.completeHook(h.ID.String(), string(box.files[path.Join(dir, "script")])) }
	tick(t, e)
	h, err = s.Hook(t.Context(), a.RunID, "after_create")
	if err != nil || h == nil || h.Status != "completed" {
		t.Fatal(h, err)
	}
}

func TestUncertainHookStartRetriesSameOperation(t *testing.T) {
	s, box, a, e := setupHooks(t, true, false)
	box.lostHookStart = true
	found := false
	for range 15 {
		err := e.Tick(t.Context())
		if errors.Is(err, harness.ErrUncertain) {
			found = true
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !found {
		t.Fatal("uncertain Start was not reached")
	}
	e.Disconnect()
	tickUntil(t, e, func() bool { return box.startAttempts == 2 })
	h, err := s.Hook(t.Context(), a.RunID, "after_create")
	if err != nil || h == nil || h.Status != "running" || len(box.invoked) != 1 {
		t.Fatal(h, box.invoked, err)
	}
	tickUntil(t, e, func() bool {
		h, err := s.Hook(t.Context(), a.RunID, "after_create")
		return err == nil && h != nil && h.Status == "completed"
	})
}

func TestHookStartRetriesAreBounded(t *testing.T) {
	s, box, a, e := setupHooks(t, false, false)
	box.alwaysLostStart = true
	reconnected := false
	for range 20 {
		err := e.Tick(t.Context())
		if err != nil && !errors.Is(err, harness.ErrUncertain) {
			t.Fatal(err)
		}
		h, err := s.Hook(t.Context(), a.RunID, "after_create")
		if err != nil {
			t.Fatal(err)
		}
		if h != nil && h.Status == "failed" {
			if !reconnected || h.Error == nil || h.Error.Code != "hook_launch_failed" || h.StartAttempts != maxHookStartAttempts || box.startAttempts != maxHookStartAttempts {
				t.Fatal(h, box.startAttempts)
			}
			return
		}
		if box.startAttempts == 2 && !reconnected {
			e.Disconnect()
			reconnected = true
		}
	}
	t.Fatal("hook did not fail after bounded launch attempts")
}

func TestHookResultUsesWorkerTimeAndRecordedTimeout(t *testing.T) {
	s, box, a, e := setupHooks(t, false, false)
	tickUntil(t, e, func() bool { return len(box.invoked) == 1 })
	h, err := s.Hook(t.Context(), a.RunID, "after_create")
	if err != nil || h == nil {
		t.Fatal(h, err)
	}
	box.completeHook(h.ID.String(), "done")
	dir := path.Join("/home/template/.orpheus/hooks", h.ID.String())
	var file hookResultFile
	if err := json.Unmarshal(box.files[path.Join(dir, "result.json")], &file); err != nil {
		t.Fatal(err)
	}
	file.FinishedAt = time.Now().Add(24 * time.Hour)
	box.files[path.Join(dir, "result.json")], _ = json.Marshal(file)
	tick(t, e)
	h, err = s.Hook(t.Context(), a.RunID, "after_create")
	if err != nil || h == nil || h.Status != "completed" || h.FinishedAt == nil || h.FinishedAt.After(time.Now().Add(time.Minute)) {
		t.Fatal(h, err)
	}
}

func TestHookResultHonorsRecordedStopReason(t *testing.T) {
	for _, tc := range []struct {
		reason string
		status string
	}{
		{"timeout", "failed"},
		{"cancelled", "cancelled"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			s, box, a, e := setupHooks(t, false, false)
			tickUntil(t, e, func() bool { return len(box.invoked) == 1 })
			h, err := s.Hook(t.Context(), a.RunID, "after_create")
			if err != nil || h == nil {
				t.Fatal(h, err)
			}
			if err := s.ChangeHook(t.Context(), a.SessionID, a.RunID, "after_create", func(h *store.HookExecution, _ *store.RunRecord) error {
				h.StopReason = &tc.reason
				h.CancelAttemptedAt = new(time.Now().UTC())
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			box.completeHook(h.ID.String(), "done")
			tick(t, e)
			h, err = s.Hook(t.Context(), a.RunID, "after_create")
			if err != nil || h == nil || h.Status != tc.status {
				t.Fatal(h, err)
			}
		})
	}
}

func TestCancellingPreparationStopsOnlyHook(t *testing.T) {
	s, box, a, e := setupHooks(t, false, false)
	tickUntil(t, e, func() bool { return len(box.invoked) == 1 })
	hook, err := s.Hook(t.Context(), a.RunID, "after_create")
	if err != nil || hook == nil {
		t.Fatal(hook, err)
	}
	dir := path.Join("/home/template/.orpheus/hooks", hook.ID.String())
	started, _ := json.Marshal(hookStarted{OperationID: hook.ID.String(), WrapperPID: 200, HookPID: new(201)})
	box.files[path.Join(dir, "started.json")] = started
	box.processes = append(box.processes, harness.Process{PID: 200, Env: map[string]string{"ORPHEUS_HOOK_OPERATION_ID": hook.ID.String()}})
	if _, err := s.Cancel(t.Context(), a.SessionID, a.RunID); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	if len(box.signals) != 1 || !strings.Contains(box.signals[0], "-TERM 201") {
		t.Fatal("hook did not receive SIGTERM", box.signals)
	}
	box.processes = box.processes[:0]
	result, _ := json.Marshal(hookResultFile{OperationID: hook.ID.String(), FinishedAt: time.Now().UTC(), Signal: new(15), OutputCompleteness: "complete", HeadFile: "head.txt"})
	box.files[path.Join(dir, "result.json")] = result
	box.files[path.Join(dir, "head.txt")] = nil
	tickUntil(t, e, func() bool {
		run, err := s.Run(t.Context(), a.SessionID, a.RunID)
		return err == nil && run.Status == session.Cancelled
	})
	run, err := s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || box.starts != 0 || len(box.invoked) != 1 || run.Hooks[0].Status != "cancelled" || run.Hooks[1].Status != "skipped" || run.Hooks[2].Status != "skipped" {
		t.Fatal(run, box.invoked, err)
	}
}

func TestHookTimeoutFailsPreparationWithoutAgent(t *testing.T) {
	s, box, a, e := setupHooks(t, false, false)
	tickUntil(t, e, func() bool { return len(box.invoked) == 1 })
	hook, err := s.Hook(t.Context(), a.RunID, "after_create")
	if err != nil || hook == nil {
		t.Fatal(hook, err)
	}
	dir := path.Join("/home/template/.orpheus/hooks", hook.ID.String())
	started, _ := json.Marshal(hookStarted{OperationID: hook.ID.String(), WrapperPID: 200, HookPID: new(201)})
	box.files[path.Join(dir, "started.json")] = started
	box.processes = append(box.processes, harness.Process{PID: 200, Env: map[string]string{"ORPHEUS_HOOK_OPERATION_ID": hook.ID.String()}})
	if _, err := s.Pool.Exec(t.Context(), "UPDATE hook_executions SET deadline_at=now()-interval '1 second' WHERE id=$1", hook.ID); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	if len(box.signals) != 1 || !strings.Contains(box.signals[0], "-TERM 201") {
		t.Fatal(box.signals)
	}
	result, _ := json.Marshal(hookResultFile{OperationID: hook.ID.String(), FinishedAt: time.Now().UTC(), Signal: new(15), OutputCompleteness: "complete", HeadFile: "head.txt"})
	box.files[path.Join(dir, "result.json")] = result
	box.files[path.Join(dir, "head.txt")] = nil
	box.processes = box.processes[:0]
	tickUntil(t, e, func() bool {
		run, err := s.Run(t.Context(), a.SessionID, a.RunID)
		return err == nil && run.Status.Terminal()
	})
	run, err := s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || run.Status != session.Failed || run.Error == nil || run.Error.Code != "hook_timeout" || box.starts != 0 || run.Hooks[0].Status != "failed" || run.Hooks[2].Status != "skipped" {
		t.Fatal(run, err)
	}
}

func TestHookForceStopHasHardDeadline(t *testing.T) {
	s, box, a, e := setupHooks(t, false, false)
	tickUntil(t, e, func() bool { return len(box.invoked) == 1 })
	h, err := s.Hook(t.Context(), a.RunID, "after_create")
	if err != nil || h == nil {
		t.Fatal(h, err)
	}
	dir := path.Join("/home/template/.orpheus/hooks", h.ID.String())
	started, _ := json.Marshal(hookStarted{OperationID: h.ID.String(), WrapperPID: 200, HookPID: new(201)})
	box.files[path.Join(dir, "started.json")] = started
	box.processes = append(box.processes, harness.Process{PID: 200, Env: map[string]string{"ORPHEUS_HOOK_OPERATION_ID": h.ID.String()}})
	if err := s.ChangeHook(t.Context(), a.SessionID, a.RunID, "after_create", func(h *store.HookExecution, _ *store.RunRecord) error {
		h.DeadlineAt = new(time.Now().Add(-time.Minute))
		h.CancelAttemptedAt = new(time.Now().Add(-time.Minute))
		h.StopReason = new("timeout")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	h, err = s.Hook(t.Context(), a.RunID, "after_create")
	if err != nil || h == nil || h.Status != "running" || h.KillAttemptedAt == nil || len(box.killed) != 0 || len(box.signals) != 1 || !strings.Contains(box.signals[0], "-KILL 201") {
		t.Fatal(h, box.killed, box.signals, err)
	}
	e.Disconnect()
	tick(t, e)
	if len(box.signals) != 1 || len(box.killed) != 0 {
		t.Fatal("forced stop was repeated after reconnect", box.signals, box.killed)
	}
	output := []byte("hook output before kill")
	box.files[path.Join(dir, "head.txt")] = output
	result, _ := json.Marshal(hookResultFile{OperationID: h.ID.String(), FinishedAt: time.Now().UTC(), Signal: new(9), OutputCompleteness: "complete", OriginalBytes: int64(len(output)), HeadFile: "head.txt"})
	box.files[path.Join(dir, "result.json")] = result
	tick(t, e)
	h, err = s.Hook(t.Context(), a.RunID, "after_create")
	if err != nil || h == nil || h.Status != "failed" || h.Error == nil || h.Error.Code != "hook_timeout" || h.Signal == nil || *h.Signal != 9 || h.OutputCompleteness != "complete" || len(box.killed) != 0 {
		t.Fatal(h, box.killed, err)
	}
}

func TestHookForceStopKillsUnresponsiveWrapper(t *testing.T) {
	s, box, a, e := setupHooks(t, false, false)
	tickUntil(t, e, func() bool { return len(box.invoked) == 1 })
	h, err := s.Hook(t.Context(), a.RunID, "after_create")
	if err != nil || h == nil {
		t.Fatal(h, err)
	}
	dir := path.Join("/home/template/.orpheus/hooks", h.ID.String())
	started, _ := json.Marshal(hookStarted{OperationID: h.ID.String(), WrapperPID: 200, HookPID: new(201)})
	box.files[path.Join(dir, "started.json")] = started
	box.processes = append(box.processes, harness.Process{PID: 200, Env: map[string]string{"ORPHEUS_HOOK_OPERATION_ID": h.ID.String()}})
	if err := s.ChangeHook(t.Context(), a.SessionID, a.RunID, "after_create", func(h *store.HookExecution, _ *store.RunRecord) error {
		h.DeadlineAt = new(time.Now().Add(-time.Minute))
		h.CancelAttemptedAt = new(time.Now().Add(-time.Minute))
		h.StopReason = new("timeout")
		h.KillAttemptedAt = new(time.Now().Add(-time.Minute))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	h, err = s.Hook(t.Context(), a.RunID, "after_create")
	if err != nil || h == nil || h.Status != "failed" || h.Error == nil || h.Error.Code != "hook_timeout" || len(box.killed) != 1 || box.killed[0] != 200 {
		t.Fatal(h, box.killed, err)
	}
}

func TestSandboxLossMarksActiveHookUnavailable(t *testing.T) {
	s, box, a, e := setupHooks(t, true, false)
	tickUntil(t, e, func() bool { return box.starts == 1 })
	box.autoResult = false
	complete(box.remote)
	tickUntil(t, e, func() bool { return len(box.invoked) == 3 })
	if err := e.failure(t.Context(), &harness.ExecutionError{Code: "sandbox_lost", Message: "Sandbox is no longer available."}); err != nil {
		t.Fatal(err)
	}
	run, err := s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || run.Status != session.Failed || run.AgentStatus == nil || *run.AgentStatus != session.Completed || run.FinalMessage == nil || run.Hooks[2].Status != "failed" || run.Hooks[2].Error == nil || run.Error == nil || run.Error.Code != "sandbox_lost" {
		t.Fatal(run, err)
	}
}
