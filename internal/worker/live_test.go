//go:build live

package worker

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/abox-dev/sdk/packages/go-sdk"
	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/agentbox"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/harness/codex"
	"github.com/orpheus-agents/orpheus/internal/secret"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

type faultPlatform struct {
	harness.Platform
	ids  []string
	lost bool
}

func (p *faultPlatform) Create(ctx context.Context, template string, timeout time.Duration, metadata map[string]string) (harness.Sandbox, error) {
	box, err := p.Platform.Create(ctx, template, timeout, metadata)
	if err != nil {
		return nil, err
	}
	p.ids = append(p.ids, box.ID())
	if !p.lost {
		p.lost = true
		return nil, harness.ErrUncertain
	}
	return box, nil
}

type faultDriver struct {
	harness.Driver
	lost map[string]bool
}

func (d *faultDriver) Launch(ctx context.Context, env map[string]string, cwd string, source session.Credentials) (int, error) {
	pid, err := d.Driver.Launch(ctx, env, cwd, source)
	if err == nil && !d.lost["launch"] {
		d.lost["launch"] = true
		return 0, harness.ErrUncertain
	}
	return pid, err
}
func (d *faultDriver) Start(ctx context.Context, agent session.AgentConfiguration, thread string, texts []string) (string, error) {
	id, err := d.Driver.Start(ctx, agent, thread, texts)
	if err == nil && !d.lost["start"] {
		d.lost["start"] = true
		return "", harness.ErrUncertain
	}
	return id, err
}
func (d *faultDriver) Steer(ctx context.Context, thread, turn, text string) error {
	err := d.Driver.Steer(ctx, thread, turn, text)
	if err == nil && !d.lost["steer"] {
		d.lost["steer"] = true
		return harness.ErrUncertain
	}
	return err
}
func TestLiveCodexRecoveryPauseResume(t *testing.T) {
	for _, name := range []string{"AGENTBOX_API_KEY", "OPENAI_API_KEY"} {
		if os.Getenv(name) == "" {
			t.Fatalf("%s is required for live tests", name)
		}
	}
	platform, err := agentbox.New()
	if err != nil {
		t.Fatal(err)
	}
	faults := &faultPlatform{Platform: platform}
	sdkClient, err := sdk.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, id := range faults.ids {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, err := sdkClient.Sandboxes.Kill(ctx, id)
			cancel()
			if err != nil {
				t.Error("sandbox cleanup failed")
			}
		}
	})
	cipher, _ := secret.New(base64.URLEncoding.EncodeToString(make([]byte, 32)))
	settings := config.DefaultSettings()
	settings.WorkerPoll = time.Second
	runnerDir := t.TempDir()
	for _, arch := range []string{"amd64", "arm64"} {
		binary := filepath.Join(runnerDir, "hook-runner-"+arch)
		build := exec.CommandContext(t.Context(), "go", "build", "-trimpath", "-o", binary, "github.com/orpheus-agents/orpheus/cmd/orpheus-hook-runner")
		build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build hook runner: %v: %s", err, output)
		}
	}
	model := os.Getenv("ORPHEUS_TEST_MODEL")
	if model == "" {
		model = "gpt-5.4"
	}
	s := &store.Store{Pool: testutil.Database(t), Settings: settings, Cipher: cipher, Profiles: config.Profiles{Profiles: map[string]config.Profile{"live": {Harness: "codex", Model: &model, Auth: config.Auth{Mode: "api_key", APIKeyEnv: "OPENAI_API_KEY"}}}}}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	hooks := &session.HooksInput{AfterCreate: new("#!/bin/sh\nprintf 'created\\n'\n"), BeforeRun: new("#!/bin/sh\nprintf 'prepared\\n'\n"), AfterRun: new("#!/bin/sh\nprintf 'agent:%s\\n' \"$ORPHEUS_AGENT_STATUS\"\n")}
	a, err := s.Accept(ctx, store.Admission{Key: uuid.New(), Create: &session.CreateSession{Configuration: session.ConfigurationInput{Agent: session.AgentInput{Profile: "live"}, Sandbox: session.SandboxInput{Template: "codex"}, Limits: session.Limits{RunTimeoutSeconds: 300}, Hooks: hooks}, Messages: []session.TextMessage{{Text: "Work only in the current directory."}, {Text: "Do not access other directories or networks."}, {Text: "Create a file named orpheus-probe.txt in the current directory containing exactly ORPHEUS_OK. Then run sleep 20 in the shell to allow a follow-up. Finally include CREATED in your reply."}}}})
	if err != nil {
		t.Fatal(err)
	}
	lost := map[string]bool{}
	newExecutor := func() *Executor {
		e := NewExecutor(a.SessionID, s, faults)
		e.RunnerBinary = func(remotePath string) ([]byte, error) {
			return os.ReadFile(filepath.Join(runnerDir, filepath.Base(remotePath)))
		}
		e.NewDriver = func(box harness.Sandbox) harness.Driver {
			return &faultDriver{Driver: codex.New(box, settings.RPCTimeout, settings.MaxToolResultBytes), lost: lost}
		}
		return e
	}
	e := newExecutor()
	defer func() { e.Disconnect() }()
	await := func(id uuid.UUID, marker string) {
		t.Helper()
		restarted := false
		steered := false
		lastStatus := session.Status("")
		lastObservation := ""
		var lastRunError *session.Error
		var lastTickError error
		for ctx.Err() == nil {
			err := e.Tick(ctx)
			lastTickError = err
			if err != nil {
				if f, ok := errors.AsType[*harness.ExecutionError](err); ok {
					if err := e.failure(ctx, f); err != nil {
						t.Fatal(err)
					}
				} else {
					e.Disconnect()
				}
			}
			run, err := s.Run(ctx, a.SessionID, id)
			if err != nil {
				t.Fatal(err)
			}
			observation := ""
			if run.Observation != nil {
				observation = *run.Observation
			}
			if run.Status != lastStatus || observation != lastObservation {
				t.Logf("live run status=%s observation=%s faults=%v", run.Status, observation, lost)
			}
			lastStatus, lastObservation, lastRunError = run.Status, observation, run.Error
			if run.Status.Terminal() {
				if run.Status != session.Completed || run.FinalMessage == nil || !strings.Contains(run.FinalMessage.Text, marker) || id == a.RunID && !strings.Contains(run.FinalMessage.Text, "STEER_OK") || len(run.Hooks) == 0 || run.Hooks[len(run.Hooks)-1].Status != "completed" {
					t.Fatalf("unexpected live run outcome: status=%s error=%v", run.Status, run.Error)
				}
				for ctx.Err() == nil {
					if err := e.Tick(ctx); err != nil {
						t.Fatal(err)
					}
					record, _, err := s.Read(ctx, a.SessionID)
					if err != nil {
						t.Fatal(err)
					}
					if !record.SlotReserved {
						return
					}
					time.Sleep(time.Second)
				}
				break
			}
			if id == a.RunID && run.Status == session.Running && !steered {
				_, err := s.Accept(ctx, store.Admission{SessionID: a.SessionID, RunID: a.RunID, Key: uuid.New(), Messages: []session.TextMessage{{Text: "This is a follow-up for the current turn."}, {Text: "Keep the file contents exactly ORPHEUS_OK."}, {Text: "After the sleep, include STEER_OK with CREATED in your final reply."}}})
				if err != nil {
					t.Fatal(err)
				}
				steered = true
			}
			if run.Status == session.Running && !restarted {
				e.Disconnect()
				e = newExecutor()
				restarted = true
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
		t.Fatalf("live run timed out: status=%s observation=%s error=%v last_tick=%v faults=%v restarted=%t steered=%t", lastStatus, lastObservation, lastRunError, lastTickError, lost, restarted, steered)
	}
	await(a.RunID, "CREATED")
	completed, err := s.Run(ctx, a.SessionID, a.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.FinalMessage == nil {
		t.Fatal("completed run has no final message")
	}
	var items []session.HistoryItem
	for cursor := ""; ; {
		history, err := s.History(ctx, a.SessionID, &a.RunID, 100, cursor, nil)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, history.Items...)
		if history.NextCursor == nil {
			break
		}
		cursor = *history.NextCursor
	}
	want := []string{"Work only in the current directory.", "Do not access other directories or networks.", "Create a file named orpheus-probe.txt in the current directory containing exactly ORPHEUS_OK. Then run sleep 20 in the shell to allow a follow-up. Finally include CREATED in your reply.", "This is a follow-up for the current turn.", "Keep the file contents exactly ORPHEUS_OK.", "After the sleep, include STEER_OK with CREATED in your final reply."}
	var got []string
	lastUserIndex, finalIndex := -1, -1
	for index, item := range items {
		if item.Type != "message" || item.Message == nil {
			continue
		}
		if item.Message.Role == "user" {
			got = append(got, item.Message.Text)
			lastUserIndex = index
		}
		if item.Message.ID == completed.FinalMessage.ID {
			finalIndex = index
		}
	}
	if !slices.Equal(got, want) || finalIndex <= lastUserIndex || !lost["steer"] {
		t.Fatalf("live batched input or recovery failed: messages=%q final_index=%d last_user_index=%d steer_ack_lost=%t", got, finalIndex, lastUserIndex, lost["steer"])
	}
	b, err := s.Accept(ctx, store.Admission{SessionID: a.SessionID, Key: uuid.New(), Messages: []session.TextMessage{{Text: "Read orpheus-probe.txt from the current directory and reply with only its contents."}}})
	if err != nil {
		t.Fatal(err)
	}
	await(b.RunID, "ORPHEUS_OK")
	if len(faults.ids) != 1 || !lost["launch"] || !lost["start"] || !lost["steer"] {
		t.Fatal("fault/recovery contract not exercised")
	}
}
