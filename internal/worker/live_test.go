//go:build live

package worker

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/abox-dev/sdk/packages/go-sdk"
	"github.com/google/uuid"
	"github.com/skillum-ai/orpheus/internal/agentbox"
	"github.com/skillum-ai/orpheus/internal/config"
	"github.com/skillum-ai/orpheus/internal/harness"
	"github.com/skillum-ai/orpheus/internal/harness/codex"
	"github.com/skillum-ai/orpheus/internal/secret"
	"github.com/skillum-ai/orpheus/internal/session"
	"github.com/skillum-ai/orpheus/internal/store"
	"github.com/skillum-ai/orpheus/internal/testutil"
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

func (d *faultDriver) Launch(ctx context.Context, env map[string]string, cwd string) (int, error) {
	pid, err := d.Driver.Launch(ctx, env, cwd)
	if err == nil && !d.lost["launch"] {
		d.lost["launch"] = true
		return 0, harness.ErrUncertain
	}
	return pid, err
}
func (d *faultDriver) Start(ctx context.Context, thread, text string) (string, error) {
	id, err := d.Driver.Start(ctx, thread, text)
	if err == nil && !d.lost["start"] {
		d.lost["start"] = true
		return "", harness.ErrUncertain
	}
	return id, err
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
		build := exec.CommandContext(t.Context(), "go", "build", "-trimpath", "-o", binary, "github.com/skillum-ai/orpheus/cmd/orpheus-hook-runner")
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
	a, err := s.Accept(ctx, store.Admission{Key: uuid.New(), Create: &session.CreateSession{Configuration: session.ConfigurationInput{Agent: session.AgentInput{Profile: "live"}, Sandbox: session.SandboxInput{Template: "codex"}, Limits: session.Limits{RunTimeoutSeconds: 300}, Hooks: hooks}, Message: session.TextMessage{Text: "Create a file named orpheus-probe.txt in the current directory containing exactly ORPHEUS_OK. Then reply with exactly CREATED. Do not access other directories or networks."}}})
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
		for ctx.Err() == nil {
			err := e.Tick(ctx)
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
			if run.Status.Terminal() {
				if run.Status != session.Completed || run.FinalMessage == nil || !strings.Contains(run.FinalMessage.Text, marker) || len(run.Hooks) == 0 || run.Hooks[len(run.Hooks)-1].Status != "completed" {
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
		t.Fatal("live run timed out")
	}
	await(a.RunID, "CREATED")
	b, err := s.Accept(ctx, store.Admission{SessionID: a.SessionID, Key: uuid.New(), Text: "Read orpheus-probe.txt from the current directory and reply with only its contents."})
	if err != nil {
		t.Fatal(err)
	}
	await(b.RunID, "ORPHEUS_OK")
	if len(faults.ids) != 1 || !lost["launch"] || !lost["start"] {
		t.Fatal("fault/recovery contract not exercised")
	}
}
