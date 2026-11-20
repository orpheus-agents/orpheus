//go:build live

package worker

import (
	"cmp"
	"context"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/agentbox"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/llm"
	"github.com/orpheus-agents/orpheus/internal/secret"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

//go:embed testdata/opencode_model.py
var openCodeModel string

type delayedOpenCodeDriver struct {
	*faultDriver
	box harness.Sandbox
}

func (d *delayedOpenCodeDriver) Deliver(ctx context.Context, agent session.AgentConfiguration, thread string, turn *string, in harness.Delivery) (string, error) {
	if len(in.Inputs) == 1 && in.Inputs[0].Text == "LATE_CITY" {
		// Release the native model after the worker observed the run active.
		// Its final response lands before the actual steer request.
		if _, err := d.box.Run(ctx, `touch "$HOME/workspace/late.release"; for i in $(seq 1 50); do test -e "$HOME/workspace/late.answered" && break; sleep 0.1; done; test -e "$HOME/workspace/late.answered"; sleep 1`); err != nil {
			return "", err
		}
	}
	return d.faultDriver.Deliver(ctx, agent, thread, turn, in)
}
func TestLiveOpenCodeDeliveryAndHistory(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	platform, err := agentbox.New()
	if err != nil {
		t.Fatal(err)
	}
	key, _ := secret.New(base64.URLEncoding.EncodeToString(make([]byte, 32)))
	s := &store.Store{Pool: testutil.Database(t), Settings: config.DefaultSettings(), Cipher: key}
	account, err := s.CreateAccount(ctx, llm.AccountInput{Name: "fixture", Harness: "opencode", AuthMode: "api_key", Provider: "fixture", APIKey: new("fixture-key")})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := s.CreateProfile(ctx, llm.ProfileInput{Name: "fixture", AccountID: account.ID, Model: "fixture", OpenCode: session.OpenCodeConfiguration{Variant: new("thorough")}, Instructions: "Use tools when requested."})
	if err != nil {
		t.Fatal(err)
	}
	template := cmp.Or(os.Getenv("ORPHEUS_TEST_OPENCODE_TEMPLATE"), "opencode")
	testutil.RegisterTemplate(t, s.Pool, template)
	hook := "#!/bin/sh\nset -eu\n"
	// Optional bootstrap for testing a pinned upstream binary before its base template ships.
	if path := os.Getenv("ORPHEUS_TEST_OPENCODE_SETUP"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		hook += string(raw) + "\n"
	}
	hook += "cat > model.py <<'PYTHON'\n" + openCodeModel + "\nPYTHON\nnohup python3 model.py </dev/null > model.log 2>&1 &\nfor i in $(seq 1 50); do test -s opencode.json && exit 0; sleep 0.1; done\nexit 1\n"
	a, err := s.Accept(ctx, store.Admission{Key: uuid.New(), Create: &session.CreateSession{AllowMultipleRuns: true, Configuration: session.ConfigurationInput{Agent: session.AgentInput{Profile: profile.Name}, Sandbox: session.SandboxInput{Template: template}, Limits: session.Limits{RunTimeoutSeconds: 240, MaxSessionTokens: 600000}, Hooks: &session.HooksInput{AfterCreate: &hook, AfterRun: new("#!/bin/sh\ntouch hook.done"), TimeoutSeconds: new(180)}}, Messages: []session.TextMessage{{Text: "SHELL_GATE"}, {Text: "Take Moscow as the initial city."}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		rec, _, err := s.Read(context.Background(), a.SessionID)
		if err == nil && rec.SandboxID != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := platform.Delete(ctx, *rec.SandboxID); err != nil && !errors.Is(err, harness.ErrNotFound) {
				t.Error(err)
			}
		}
	})
	runnerDir := t.TempDir()
	for _, arch := range []string{"amd64", "arm64"} {
		cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", filepath.Join(runnerDir, "hook-runner-"+arch), "github.com/orpheus-agents/orpheus/cmd/orpheus-hook-runner")
		cmd.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0", "GOARCH="+arch)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("runner: %s", out)
		}
	}
	lost := map[string]bool{}
	newOwner := func() *Executor {
		e := NewExecutor(a.SessionID, s, platform)
		factory := e.NewDriver
		e.NewDriver = func(box harness.Sandbox, record store.SessionRecord, rid uuid.UUID, source session.Credentials) harness.Driver {
			return &delayedOpenCodeDriver{faultDriver: &faultDriver{Driver: factory(box, record, rid, source), lost: lost}, box: box}
		}
		e.RunnerBinary = func(path string) ([]byte, error) { return os.ReadFile(filepath.Join(runnerDir, filepath.Base(path))) }
		return e
	}
	e := newOwner()
	defer func() { e.Disconnect() }()
	var action func(uuid.UUID)
	expected := session.Completed
	run := func(id uuid.UUID, want string) {
		t.Helper()
		steered := false
		for ctx.Err() == nil {
			err := e.Tick(ctx)
			if err != nil {
				if f, ok := errors.AsType[*harness.ExecutionError](err); ok {
					t.Fatalf("execution error %s %s", f.Code, f.Message)
				}
				if !errors.Is(err, harness.ErrUncertain) {
					t.Fatal(err)
				}
				e.Disconnect()
				e = newOwner()
			}
			r, err := s.Run(ctx, a.SessionID, id)
			if err != nil {
				t.Fatal(err)
			}
			if r.Status.Terminal() {
				if r.Status != expected || (expected == session.Completed && (r.FinalMessage == nil || !strings.Contains(r.FinalMessage.Text, want) || r.Usage.TotalTokens == 0)) {
					t.Fatalf("run %s status=%s error=%v final=%v usage=%v", id, r.Status, r.Error, r.FinalMessage, r.Usage)
				}
				for _, h := range r.Hooks {
					if h.Status != "completed" {
						t.Fatalf("unfinished hook: %v", h)
					}
				}
				return
			}
			if r.Status == session.Running && action != nil && e.sandbox != nil {
				action(id)
			}
			if id == a.RunID && !steered && e.sandbox != nil && r.Status == session.Running {
				gate, _ := e.sandbox.Run(ctx, `test -f "$HOME/workspace/gate.started" && echo yes`)
				if strings.TrimSpace(string(gate)) == "yes" {
					_, err = s.Accept(ctx, store.Admission{Key: uuid.New(), SessionID: a.SessionID, RunID: id, Messages: []session.TextMessage{{Text: "SAINT_PETERSBURG"}}})
					if err != nil {
						t.Fatal(err)
					}
					steered = true
				}
			}
			time.Sleep(300 * time.Millisecond)
		}
		t.Fatal(ctx.Err())
	}
	run(a.RunID, "PETERSBURG_OK")
	msgs, err := store.Messages(ctx, s.Pool, a.RunID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, m := range msgs {
		if m.Role == "user" {
			count++
			if m.DeliveryStatus == nil || *m.DeliveryStatus != "delivered" || m.NativeKey == nil {
				t.Fatalf("missing proof: %v", m)
			}
		}
	}
	if count != 3 || !lost["start"] || !lost["steer"] || !lost["launch"] {
		t.Fatal(count, lost)
	}
	history, err := s.History(ctx, a.SessionID, &a.RunID, 100, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	tool := false
	for _, item := range history.Items {
		if item.Type == "tool_call" {
			tool = true
		}
	}
	if !tool {
		t.Fatalf("missing tool in history: %v", history.Items)
	}
	first, _, err := s.Read(ctx, a.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	for ctx.Err() == nil {
		if err = e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		r, _, err := s.Read(ctx, a.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if r.SandboxState == "paused" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	b, err := s.Accept(ctx, store.Admission{Key: uuid.New(), SessionID: a.SessionID, Messages: []session.TextMessage{{Text: "SECOND_RUN"}}})
	if err != nil {
		t.Fatal(err)
	}
	run(b.RunID, "HISTORY_OK")
	last, _, err := s.Read(ctx, a.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if *first.LaunchID == *last.LaunchID || *first.ThreadID != *last.ThreadID || *first.SandboxID != *last.SandboxID {
		t.Fatal("run isolation or history changed")
	}
	submit := func(text string) uuid.UUID {
		t.Helper()
		a2, err := s.Accept(ctx, store.Admission{Key: uuid.New(), SessionID: a.SessionID, Messages: []session.TextMessage{{Text: text}}})
		if err != nil {
			t.Fatal(err)
		}
		return a2.RunID
	}
	gate := func(name string) bool {
		raw, _ := e.sandbox.Run(ctx, `test -f "$HOME/workspace/`+name+`" && echo yes`)
		return strings.TrimSpace(string(raw)) == "yes"
	}
	steer := func(id uuid.UUID, text string) uuid.UUID {
		item, err := s.Accept(ctx, store.Admission{Key: uuid.New(), SessionID: a.SessionID, RunID: id, Messages: []session.TextMessage{{Text: text}}})
		if err != nil {
			t.Fatal(err)
		}
		return item.MessageID
	}
	verifyRejected := func(id, mid uuid.UUID) {
		t.Helper()
		messages, err := store.Messages(ctx, s.Pool, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range messages {
			if m.ID == mid && m.DeliveryStatus != nil && *m.DeliveryStatus == "rejected" && m.Error != nil && m.Error.Code == "run_finished_before_delivery" {
				return
			}
		}
		t.Fatal("missing proven undelivered input", mid)
	}
	var lateID uuid.UUID
	action = func(id uuid.UUID) {
		if gate("late.started") {
			lateID = steer(id, "LATE_CITY")
			action = nil
		}
	}
	late := submit("LATE_CASE")
	run(late, "HISTORY_OK")
	verifyRejected(late, lateID)
	var unseen uuid.UUID
	action = func(id uuid.UUID) {
		if !gate("crash.started") {
			return
		}
		if unseen == uuid.Nil {
			unseen = steer(id, "UNSEEN_AFTER_CRASH")
			return
		}
		// Kill only this test's server and its recorded owner in the disposable box.
		record, _, err := s.Read(ctx, a.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = e.sandbox.Run(ctx, fmt.Sprintf("children=$(pgrep -P %d) && kill -KILL $children %d", *record.ProcessID, *record.ProcessID)); err != nil {
			t.Fatal(err)
		}
		e.Disconnect()
		e = newOwner()
		action = nil
	}
	crash := submit("CRASH_CASE")
	expected = session.Failed
	run(crash, "")
	verifyRejected(crash, unseen)
	expected = session.Cancelled
	action = func(id uuid.UUID) {
		if gate("cancel.started") {
			_, err := s.Cancel(ctx, a.SessionID, id)
			if err != nil {
				t.Fatal(err)
			}
			action = nil
		}
	}
	cancelled := submit("CANCEL_CASE")
	run(cancelled, "")
	expected = session.Completed
	comp := submit("COMPACT_CASE " + strings.Repeat(" history-segment", 12000))
	run(comp, "COMPACT_OK")
	if !gate("compacted") {
		t.Fatal("model never received a compaction request")
	}
	old, err := s.History(ctx, a.SessionID, &a.RunID, 100, "", nil)
	if err != nil || len(old.Items) != len(history.Items) {
		t.Fatal("lost pre-compaction history", err)
	}
	if gate("cancel.finished") {
		t.Fatal("cancelled tool completed")
	}
	raw, err := e.sandbox.Run(ctx, `wc -c < "$HOME/workspace/crash.count"`)
	if err != nil || strings.TrimSpace(string(raw)) != "1" {
		t.Fatal("replayed interrupted tool", err)
	}

	expected = session.Failed
	failed := submit("FAIL_AUTH_CASE")
	run(failed, "")
	failedRun, err := s.Run(ctx, a.SessionID, failed)
	if err != nil || failedRun.Error == nil || failedRun.Error.Code != "authentication_failed" {
		t.Fatalf("authentication result: %v %v", failedRun.Error, err)
	}
	expected = session.Cancelled
	budget := submit("BUDGET_CASE")
	run(budget, "")
	limited, err := s.Run(ctx, a.SessionID, budget)
	if err != nil || limited.StopReason == nil || *limited.StopReason != "token_limit" || limited.Usage.InputTokens < 600000 {
		t.Fatalf("budget: %v %v %v", limited.StopReason, limited.Usage, err)
	}

}
