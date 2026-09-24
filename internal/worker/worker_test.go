//go:build integration

package worker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/credentials"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/secret"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

type remote struct {
	harness.Sandbox
	state                                                                                  string
	metadata                                                                               map[string]string
	processes                                                                              []harness.Process
	turns                                                                                  []harness.Turn
	usage                                                                                  []harness.UsageReport
	creates, launches, starts, steers, cancels, pauses, resumes, renewals, opens, attaches int
	seeds, syncs                                                                           int
	lostCreate, lostLaunch, lostStart, lostSteer                                           bool
	pauseError, initializeError, prepareError                                              error
	createError, startError, steerError, contextError, snapshotError, leaseError           error
	logins                                                                                 []bool
	pauseHook                                                                              func()
	killed                                                                                 []int
	env                                                                                    map[string]string
	maxTimeout                                                                             time.Duration
	timeouts                                                                               []time.Duration
}

func (r *remote) ID() string { return "sandbox" }
func (r *remote) Create(_ context.Context, _ string, timeout time.Duration, meta map[string]string) (harness.Sandbox, error) {
	r.creates++
	if err := r.checkTimeout(timeout); err != nil {
		return nil, err
	}
	if r.createError != nil {
		return nil, r.createError
	}
	r.metadata = maps.Clone(meta)
	r.state = "running"
	if r.lostCreate {
		r.lostCreate = false
		return nil, harness.ErrUncertain
	}
	return r, nil
}
func (r *remote) Find(_ context.Context, meta map[string]string) ([]string, error) {
	if maps.Equal(meta, r.metadata) {
		return []string{"sandbox"}, nil
	}
	return nil, nil
}
func (r *remote) Info(context.Context, string) (string, error) {
	if r.state == "lost" {
		return "", harness.ErrNotFound
	}
	return r.state, nil
}
func (r *remote) Connect(_ context.Context, _ string, timeout time.Duration) (harness.Sandbox, error) {
	r.resumes++
	if err := r.checkTimeout(timeout); err != nil {
		return nil, err
	}
	r.state = "running"
	return r, nil
}
func (r *remote) Pause(context.Context) error {
	r.pauses++
	if r.pauseHook != nil {
		r.pauseHook()
	}
	if r.pauseError != nil {
		return r.pauseError
	}
	r.state = "paused"
	return nil
}
func (r *remote) SetTimeout(_ context.Context, timeout time.Duration) error {
	r.renewals++
	if err := r.checkTimeout(timeout); err != nil {
		return err
	}
	return r.leaseError
}
func (r *remote) checkTimeout(timeout time.Duration) error {
	r.timeouts = append(r.timeouts, timeout)
	if r.maxTimeout > 0 && (timeout <= 0 || timeout > r.maxTimeout) {
		return harness.ErrEnvironmentRejected
	}
	return nil
}
func (r *remote) Processes(context.Context) ([]harness.Process, error) {
	return slices.Clone(r.processes), nil
}
func (r *remote) Kill(_ context.Context, pid int) error {
	r.killed = append(r.killed, pid)
	r.processes = slices.DeleteFunc(r.processes, func(p harness.Process) bool { return p.PID == pid })
	return nil
}
func (r *remote) Run(context.Context, string) ([]byte, error) {
	return []byte("/home/template/workspace\x00/home/template/.orpheus-codex\x00"), nil
}
func (r *remote) Read(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("{}")), nil
}

type fakeDriver struct {
	harness.Driver
	r *remote
}

func (d *fakeDriver) Prepare(context.Context, string, session.Credentials) (map[string]string, error) {
	return map[string]string{"CODEX_HOME": "/home/template/.orpheus-codex"}, d.r.prepareError
}
func (d *fakeDriver) Launch(_ context.Context, env map[string]string, _ string) (int, error) {
	r := d.r
	r.launches++
	r.env = maps.Clone(env)
	pid := 100 + r.launches
	r.processes = append(r.processes, harness.Process{PID: pid, Env: maps.Clone(env)})
	if r.lostLaunch {
		r.lostLaunch = false
		return 0, harness.ErrUncertain
	}
	return pid, nil
}
func (d *fakeDriver) Attach(context.Context, int) error { d.r.attaches++; return nil }
func (d *fakeDriver) Initialize(_ context.Context, _ session.Credentials, login bool) error {
	d.r.logins = append(d.r.logins, login)
	return d.r.initializeError
}
func (d *fakeDriver) OpenContext(_ context.Context, _ session.AgentConfiguration, _ string, id *string) (harness.Context, error) {
	d.r.opens++
	if d.r.contextError != nil {
		return harness.Context{}, d.r.contextError
	}
	return harness.Context{NativeID: "thread", HistoryPath: new("/history")}, nil
}
func (d *fakeDriver) HasUpdates() bool { return true }
func (d *fakeDriver) Committed()       { d.r.usage = nil }
func (d *fakeDriver) Start(_ context.Context, _ string, text string) (string, error) {
	r := d.r
	r.starts++
	if r.startError != nil {
		return "", r.startError
	}
	id := fmt.Sprintf("turn-%d", r.starts)
	r.turns = append(r.turns, harness.Turn{NativeID: id, Status: session.Running, Items: []harness.Item{{NativeID: id + "-user", Type: "user", Index: 0, Text: text}}})
	if r.lostStart {
		r.lostStart = false
		return "", harness.ErrUncertain
	}
	return id, nil
}
func (d *fakeDriver) Steer(_ context.Context, _ string, tid, text string) error {
	r := d.r
	r.steers++
	if r.steerError != nil {
		return r.steerError
	}
	for i := range r.turns {
		if r.turns[i].NativeID == tid {
			r.turns[i].Items = append(r.turns[i].Items, harness.Item{NativeID: fmt.Sprintf("steer-%d", r.steers), Type: "user", Text: text, Index: len(r.turns[i].Items)})
		}
	}
	if r.lostSteer {
		r.lostSteer = false
		return harness.ErrUncertain
	}
	return nil
}
func (d *fakeDriver) Interrupt(_ context.Context, _ string, tid string) error {
	d.r.cancels++
	return nil
}
func (d *fakeDriver) Snapshot(context.Context, string, *string, int64) (harness.Snapshot, error) {
	if d.r.snapshotError != nil {
		return harness.Snapshot{}, d.r.snapshotError
	}
	return harness.Snapshot{Usage: d.r.usage, Turns: d.r.turns, Path: new("/history"), Offset: 100}, nil
}
func (d *fakeDriver) Recover(context.Context, *string, *string, *string) (harness.Snapshot, error) {
	return d.Snapshot(context.Background(), "", nil, 0)
}
func (d *fakeDriver) Close() error { return nil }

type fakeAccount struct{ r *remote }

func (a *fakeAccount) Seed(context.Context) error  { a.r.seeds++; return nil }
func (a *fakeAccount) Watch(context.Context) error { return nil }
func (a *fakeAccount) Sync(context.Context, bool)  { a.r.syncs++ }
func (a *fakeAccount) Close() error                { return nil }
func setup(t *testing.T) (*store.Store, *remote, session.Acceptance, *Executor) {
	return setupWithEnvironment(t, session.SandboxInput{Template: "codex"}, nil)
}
func setupWithEnvironment(t *testing.T, sandbox session.SandboxInput, runEnv map[string]string) (*store.Store, *remote, session.Acceptance, *Executor) {
	t.Helper()
	pool := testutil.Database(t)
	c, _ := secret.New(base64.URLEncoding.EncodeToString(make([]byte, 32)))
	settings := config.DefaultSettings()
	settings.DatabaseURL = pool.Config().ConnString()
	settings.WorkerPoll = time.Millisecond
	s := &store.Store{Pool: pool, Settings: settings, Cipher: c, Profiles: config.Profiles{Profiles: map[string]config.Profile{"p": {Harness: "codex", Model: new("model"), Auth: config.Auth{Mode: "api_key", APIKeyEnv: "KEY"}}}}}
	a, err := s.Accept(t.Context(), store.Admission{Key: uuid.New(), Create: &session.CreateSession{Configuration: session.ConfigurationInput{Agent: session.AgentInput{Profile: "p"}, Sandbox: sandbox, Limits: session.Limits{RunTimeoutSeconds: 3600}}, Message: session.TextMessage{Text: "task"}, Env: runEnv}})
	if err != nil {
		t.Fatal(err)
	}
	r := &remote{}
	e := executor(a.SessionID, s, r)
	t.Cleanup(e.Disconnect)
	return s, r, a, e
}
func executor(id uuid.UUID, s *store.Store, r *remote) *Executor {
	e := NewExecutor(id, s, r)
	e.NewDriver = func(harness.Sandbox) harness.Driver { return &fakeDriver{r: r} }
	e.NewAccount = func(context.Context, harness.Sandbox, string, session.Credentials) (credentials.Sync, error) {
		return &fakeAccount{r}, nil
	}
	return e
}
func tick(t *testing.T, e *Executor) {
	t.Helper()
	if err := e.Tick(t.Context()); err != nil {
		if f, ok := errors.AsType[*harness.ExecutionError](err); ok {
			if err := e.failure(t.Context(), f); err != nil {
				t.Fatal(err)
			}
		} else {
			t.Fatal(err)
		}
	}
}
func complete(r *remote) {
	i := len(r.turns) - 1
	r.turns[i].Status = session.Completed
	r.turns[i].Items = append(r.turns[i].Items, harness.Item{NativeID: r.turns[i].NativeID + "-answer", Type: "assistant", Kind: new("answer"), Text: "DONE", Index: len(r.turns[i].Items)})
}
func TestFullCyclePauseResume(t *testing.T) {
	s, r, a, e := setup(t)
	tick(t, e)
	if r.creates != 1 || r.starts != 1 || r.launches != 1 {
		t.Fatal(r)
	}
	complete(r)
	tick(t, e)
	tick(t, e)
	view, err := s.Session(t.Context(), a.SessionID)
	if err != nil || view.Status != session.Completed || view.FinalMessage == nil || view.FinalMessage.Text != "DONE" || view.Sandbox.State != "paused" {
		t.Fatal(view, err)
	}
	if _, err := s.Accept(t.Context(), store.Admission{SessionID: a.SessionID, Key: uuid.New(), Text: "again"}); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	if r.creates != 1 || r.launches != 1 || r.starts != 2 || r.opens != 1 {
		t.Fatalf("cycle create=%d launch=%d starts=%d opens=%d", r.creates, r.launches, r.starts, r.opens)
	}
}
func TestRunEnvironmentDoesNotReachHarness(t *testing.T) {
	s, r, a, e := setupWithEnvironment(t,
		session.SandboxInput{Template: "codex", Env: map[string]string{"TOKEN": "private"}},
		map[string]string{"TOKEN": "override", "TASK_ID": "first"},
	)
	tick(t, e)
	if r.launches != 1 || r.env["TOKEN"] != "private" {
		t.Fatal("harness did not receive session environment", r.launches, r.env)
	}
	if _, ok := r.env["TASK_ID"]; ok {
		t.Fatal("run environment reached harness", r.env)
	}
	complete(r)
	tick(t, e)
	tick(t, e)
	if _, err := s.Accept(t.Context(), store.Admission{SessionID: a.SessionID, Key: uuid.New(), Text: "next", Env: map[string]string{"TOKEN": "second-override", "TASK_ID": "second"}}); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	if r.launches != 1 || r.starts != 2 || r.env["TOKEN"] != "private" {
		t.Fatal("new run changed harness environment or restarted it", r.launches, r.starts, r.env)
	}
	if _, ok := r.env["TASK_ID"]; ok {
		t.Fatal("second run environment reached harness", r.env)
	}
}
func TestSandboxAccessProjectionAndEvents(t *testing.T) {
	s, _, a, e := setup(t)
	initial, err := s.Session(t.Context(), a.SessionID)
	if err != nil || initial.Sandbox.ID != nil || initial.Sandbox.Workspace != nil {
		t.Fatal(initial.Sandbox, err)
	}
	tick(t, e)
	view, err := s.Session(t.Context(), a.SessionID)
	if err != nil || view.Sandbox.ID == nil || *view.Sandbox.ID != "sandbox" || view.Sandbox.Workspace == nil || *view.Sandbox.Workspace != "/home/template/workspace" {
		t.Fatal(view.Sandbox, err)
	}
	events, err := s.Events(t.Context(), a.SessionID, "0", 50)
	if err != nil {
		t.Fatal(err)
	}
	var snapshots []session.SandboxState
	for _, event := range events.Items {
		if event.Type != "sandbox.updated" {
			continue
		}
		var snapshot session.SandboxState
		if err := json.Unmarshal(event.Data, &snapshot); err != nil {
			t.Fatal(err)
		}
		snapshots = append(snapshots, snapshot)
	}
	if len(snapshots) < 3 || snapshots[0].ID != nil || snapshots[0].Workspace != nil || snapshots[1].ID == nil || *snapshots[1].ID != "sandbox" || snapshots[1].Workspace != nil {
		t.Fatal("missing incremental sandbox events", snapshots)
	}
	last := snapshots[len(snapshots)-1]
	if last.ID == nil || *last.ID != "sandbox" || last.Workspace == nil || *last.Workspace != "/home/template/workspace" {
		t.Fatal("workspace event missing", snapshots)
	}
	tick(t, e)
	again, err := s.Events(t.Context(), a.SessionID, "0", 50)
	if err != nil {
		t.Fatal(err)
	}
	var sandboxEvents int
	for _, event := range again.Items {
		if event.Type == "sandbox.updated" {
			sandboxEvents++
		}
	}
	if sandboxEvents != len(snapshots) {
		t.Fatal("unchanged sandbox emitted an event", len(snapshots), sandboxEvents)
	}
	if err := e.state(t.Context(), "unavailable", &session.Error{Code: "sandbox_lost", Message: "gone"}); err != nil {
		t.Fatal(err)
	}
	view, err = s.Session(t.Context(), a.SessionID)
	if err != nil || view.Sandbox.State != "unavailable" || view.Sandbox.ID == nil || *view.Sandbox.ID != "sandbox" || view.Sandbox.Workspace == nil || *view.Sandbox.Workspace != "/home/template/workspace" {
		t.Fatal("last known address lost", view.Sandbox, err)
	}
}
func TestLostAcknowledgements(t *testing.T) {
	for _, phase := range []string{"create", "launch", "start", "steer"} {
		t.Run(phase, func(t *testing.T) {
			s, r, a, e := setup(t)
			switch phase {
			case "create":
				r.lostCreate = true
			case "launch":
				r.lostLaunch = true
			case "start":
				r.lostStart = true
			case "steer":
				tick(t, e)
				r.lostSteer = true
				if _, err := s.Accept(t.Context(), store.Admission{SessionID: a.SessionID, RunID: a.RunID, Key: uuid.New(), Text: "same"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.Tick(t.Context()); !errors.Is(err, harness.ErrUncertain) {
				t.Fatalf("want uncertain, got %v", err)
			}
			e.Disconnect()
			e = executor(a.SessionID, s, r)
			defer e.Disconnect()
			tick(t, e)
			tick(t, e)
			if r.creates != 1 || r.launches != 1 || r.starts != 1 {
				t.Fatalf("repeated effects: create=%d launch=%d start=%d", r.creates, r.launches, r.starts)
			}
			if phase == "steer" && r.steers != 1 {
				t.Fatal("steer repeated")
			}
			view, err := s.Run(t.Context(), a.SessionID, a.RunID)
			if err != nil || view.Status != session.Running {
				t.Fatal(view, err)
			}
		})
	}
}
func TestCancelDeadlineAndForce(t *testing.T) {
	s, r, a, e := setup(t)
	tick(t, e)
	initial, err := s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || initial.DeadlineAt == nil {
		t.Fatal(initial, err)
	}
	e.Disconnect()
	e = executor(a.SessionID, s, r)
	defer e.Disconnect()
	tick(t, e)
	after, _ := s.Run(t.Context(), a.SessionID, a.RunID)
	if !after.DeadlineAt.Equal(*initial.DeadlineAt) {
		t.Fatal("deadline reset")
	}
	if _, err := s.Cancel(t.Context(), a.SessionID, a.RunID); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	tick(t, e)
	e.Disconnect()
	e = executor(a.SessionID, s, r)
	defer e.Disconnect()
	tick(t, e)
	if r.cancels != 1 {
		t.Fatalf("interrupt sent %d times", r.cancels)
	}
	r.processes = append(r.processes, harness.Process{PID: 999, Env: map[string]string{}})
	if err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, _ *store.SessionRecord) error {
		_, err := tx.Exec(t.Context(), "UPDATE runs SET cancel_attempted_at=now()-interval '60 seconds' WHERE id=$1", a.RunID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	run, err := s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || run.Status != session.Cancelled || run.StopMethod == nil || *run.StopMethod != "forced" {
		t.Fatal(run, err)
	}
	if slices.Contains(r.killed, 999) || r.launches != 1 {
		t.Fatal("forced stop touched a child or relaunched the harness")
	}
	if _, err := s.Accept(t.Context(), store.Admission{SessionID: a.SessionID, Key: uuid.New(), Text: "after forced stop"}); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	if r.launches != 2 || r.starts != 2 || r.opens != 2 {
		t.Fatalf("harness/context not reopened after force: launches=%d starts=%d opens=%d", r.launches, r.starts, r.opens)
	}
}
func TestAutomaticDeadline(t *testing.T) {
	s, r, a, e := setup(t)
	tick(t, e)
	if _, err := s.Pool.Exec(t.Context(), "UPDATE runs SET deadline_at=now()-interval '1 second' WHERE id=$1", a.RunID); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	run, err := s.Run(t.Context(), a.SessionID, a.RunID)
	if err != nil || run.Status != session.Cancelling || run.StopReason == nil || *run.StopReason != "run_timeout" || r.cancels != 1 {
		t.Fatal(run, err)
	}
}
func TestReservationDuringPause(t *testing.T) {
	s, r, a, e := setup(t)
	s.Settings.MaxConcurrentSessions = 1
	tick(t, e)
	complete(r)
	tick(t, e)
	r.pauseHook = func() {
		if _, err := s.Accept(t.Context(), store.Admission{SessionID: a.SessionID, Key: uuid.New(), Text: "next"}); err != nil {
			t.Fatal(err)
		}
	}
	tick(t, e)
	record, run, err := s.Read(t.Context(), a.SessionID)
	if err != nil || !record.SlotReserved || run == nil {
		t.Fatal(record, run, err)
	}
	r.pauseHook = nil
	tick(t, e)
	if r.starts != 2 {
		t.Fatal("new run lost")
	}
}
func TestPauseFailureAndSandboxLoss(t *testing.T) {
	s, r, a, e := setup(t)
	tick(t, e)
	complete(r)
	tick(t, e)
	r.pauseError = errors.New("offline")
	tick(t, e)
	tick(t, e)
	if r.pauses != 1 {
		t.Fatal("pause did not back off")
	}
	view, _ := s.Session(t.Context(), a.SessionID)
	if view.Status != session.Completed || view.Sandbox.State != "pausing" || view.Sandbox.Error == nil {
		t.Fatal(view)
	}
	r.state = "lost"
	e.pauseRetryAt = time.Time{}
	tick(t, e)
	record, _, err := s.Read(t.Context(), a.SessionID)
	if err != nil || record.SlotReserved {
		t.Fatal(record, err)
	}
}
func TestSameTextSteersAndProxy(t *testing.T) {
	s, r, a, e := setup(t)
	s.Settings.SandboxProxyURL = ""
	tick(t, e)
	if _, ok := r.env["ALL_PROXY"]; ok {
		t.Fatal("proxy injected despite empty setting")
	}
	for range 2 {
		if _, err := s.Accept(t.Context(), store.Admission{SessionID: a.SessionID, RunID: a.RunID, Key: uuid.New(), Text: "same"}); err != nil {
			t.Fatal(err)
		}
		tick(t, e)
		tick(t, e)
	}
	messages, err := store.Messages(t.Context(), s.Pool, a.RunID)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, m := range messages {
		if m.NativeKey != nil {
			keys[*m.NativeKey] = true
		}
	}
	if len(keys) != 3 || r.steers != 2 {
		t.Fatalf("keys=%v steers=%d", keys, r.steers)
	}
}
func TestRecoveryAfterCompletionAndProjectionFailure(t *testing.T) {
	s, r, a, e := setup(t)
	tick(t, e)
	complete(r)
	r.turns[0].Items[1].Kind = nil
	if err := e.Tick(t.Context()); err == nil {
		t.Fatal("invalid projection accepted")
	}
	view, _ := s.Run(t.Context(), a.SessionID, a.RunID)
	if view.Status != session.Running {
		t.Fatal("projection failure changed run outcome")
	}
	r.turns[0].Items[1].Kind = new("answer")
	r.processes = nil
	e.Disconnect()
	e = executor(a.SessionID, s, r)
	defer e.Disconnect()
	tick(t, e)
	tick(t, e)
	view, _ = s.Run(t.Context(), a.SessionID, a.RunID)
	if view.Status != session.Completed || view.FinalMessage == nil || r.launches != 1 {
		t.Fatal(view)
	}
}
func TestSingleOwner(t *testing.T) {
	s, _, _, _ := setup(t)
	owner, err := s.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Release()
	if _, err := owner.Exec(t.Context(), "SELECT pg_advisory_lock($1)", store.WorkerLock); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = owner.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", store.WorkerLock) }()
	if err := Run(t.Context(), s, &remote{}); err == nil || !strings.Contains(err.Error(), "another worker") {
		t.Fatal(err)
	}
}

func TestAccountRepairAcrossPreparationFailure(t *testing.T) {
	s, r, a, e := setup(t)
	record, _, err := s.Read(t.Context(), a.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	record.Configuration.Credentials = session.Credentials{Mode: "account", Store: &session.CredentialStore{Type: "s3", Bucket: "fixture", Region: "us-east-1"}, Key: "auth.json"}
	if _, err := s.Pool.Exec(t.Context(), "UPDATE sessions SET configuration=$2 WHERE id=$1", a.SessionID, record.Configuration); err != nil {
		t.Fatal(err)
	}
	r.initializeError = harness.Failure("authentication_failed", "Invalid account credentials.")
	tick(t, e)
	tick(t, e)
	if r.starts != 0 || r.syncs != 0 || r.seeds != 1 {
		t.Fatalf("invalid credentials started or uploaded: start=%d sync=%d seed=%d", r.starts, r.syncs, r.seeds)
	}
	r.initializeError = nil
	r.prepareError = harness.ErrRejected
	b, err := s.Accept(t.Context(), store.Admission{SessionID: a.SessionID, Key: uuid.New(), Text: "retry"})
	if err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	tick(t, e)
	tick(t, e)
	failed, err := s.Run(t.Context(), a.SessionID, b.RunID)
	if err != nil || failed.Status != session.Failed {
		t.Fatal(failed, err)
	}
	r.prepareError = nil
	if _, err := s.Accept(t.Context(), store.Admission{SessionID: a.SessionID, Key: uuid.New(), Text: "repaired"}); err != nil {
		t.Fatal(err)
	}
	tick(t, e)
	if r.starts != 1 || r.seeds < 3 {
		t.Fatalf("repair condition lost: starts=%d seeds=%d", r.starts, r.seeds)
	}
}
func TestRestartBeforePauseSyncsAccount(t *testing.T) {
	s, r, a, e := setup(t)
	tick(t, e)
	complete(r)
	tick(t, e)
	e.Disconnect()
	record, _, err := s.Read(t.Context(), a.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	record.Configuration.Credentials = session.Credentials{Mode: "account", Store: &session.CredentialStore{Type: "s3", Bucket: "fixture"}, Key: "auth.json"}
	if _, err := s.Pool.Exec(t.Context(), "UPDATE sessions SET configuration=$2 WHERE id=$1", a.SessionID, record.Configuration); err != nil {
		t.Fatal(err)
	}
	restarted := executor(a.SessionID, s, r)
	defer restarted.Disconnect()
	tick(t, restarted)
	if r.syncs != 1 || r.pauses != 1 {
		t.Fatalf("sync=%d pause=%d", r.syncs, r.pauses)
	}
}
func TestUncertainSteerObservationDoesNotAlternate(t *testing.T) {
	s, r, a, e := setup(t)
	tick(t, e)
	r.lostSteer = true
	if _, err := s.Accept(t.Context(), store.Admission{SessionID: a.SessionID, RunID: a.RunID, Key: uuid.New(), Text: "steer"}); err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(t.Context()); !errors.Is(err, harness.ErrUncertain) {
		t.Fatal(err)
	}
	// Hide the delivered native steer to simulate a persistently incomplete snapshot.
	r.turns[0].Items = r.turns[0].Items[:1]
	if err := e.observation(t.Context(), "uncertain"); err != nil {
		t.Fatal(err)
	}
	before, _, _ := s.Read(t.Context(), a.SessionID)
	tick(t, e)
	tick(t, e)
	after, _, _ := s.Read(t.Context(), a.SessionID)
	if after.NextEventSequence != before.NextEventSequence {
		t.Fatal("observation alternated while delivery remained uncertain")
	}
}

type blockingPlatform struct {
	harness.Platform
	entered   chan struct{}
	cancelled chan struct{}
}

func (p *blockingPlatform) Create(ctx context.Context, _ string, _ time.Duration, _ map[string]string) (harness.Sandbox, error) {
	close(p.entered)
	<-ctx.Done()
	close(p.cancelled)
	return nil, ctx.Err()
}
func TestOwnerConnectionLossCancelsExecutors(t *testing.T) {
	s, _, _, _ := setup(t)
	p := &blockingPlatform{entered: make(chan struct{}), cancelled: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, s, p) }()
	select {
	case <-p.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("executor did not start")
	}
	// Terminate only the dedicated connection holding this worker's advisory lock.
	var killed bool
	if err := s.Pool.QueryRow(t.Context(), `SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype='advisory' AND classid=($1::bigint >> 32)::oid AND objid=($1::bigint & 4294967295)::oid AND granted`, store.WorkerLock).Scan(&killed); err != nil || !killed {
		t.Fatal("owner termination", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("ownership loss was not reported")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker continued after ownership loss")
	}
	select {
	case <-p.cancelled:
	default:
		t.Fatal("in-flight external request survived ownership loss")
	}
	var locked bool
	conn, err := s.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if err := conn.QueryRow(t.Context(), "SELECT pg_try_advisory_lock($1)", store.WorkerLock).Scan(&locked); err != nil || !locked {
		t.Fatal("new owner cannot acquire lock", err)
	}
	_, _ = conn.Exec(t.Context(), "SELECT pg_advisory_unlock($1)", store.WorkerLock)
}
