//go:build live

package worker

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"testing"
	"time"

	sdk "github.com/abox-dev/sdk/packages/go-sdk"
	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/agentbox"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/secret"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

func TestLiveCodexTokenBudget(t *testing.T) {
	for _, name := range []string{"AGENTBOX_API_KEY", "OPENAI_API_KEY"} {
		if os.Getenv(name) == "" {
			t.Fatalf("%s is required for live tests", name)
		}
	}
	platform, err := agentbox.New()
	if err != nil {
		t.Fatal(err)
	}
	client, err := sdk.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.New(base64.URLEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	model := os.Getenv("ORPHEUS_TEST_MODEL")
	if model == "" {
		model = "gpt-5.4"
	}
	s := &store.Store{Pool: testutil.Database(t), Settings: config.DefaultSettings(), Cipher: cipher, Profiles: config.Profiles{Profiles: map[string]config.Profile{"live": {Harness: "codex", Model: &model, Auth: config.Auth{Mode: "api_key", APIKeyEnv: "OPENAI_API_KEY"}}}}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	a, err := s.Accept(ctx, store.Admission{Key: uuid.New(), Create: &session.CreateSession{
		Configuration: session.ConfigurationInput{Agent: session.AgentInput{Profile: "live"}, Sandbox: session.SandboxInput{Template: "codex"}, Limits: session.Limits{RunTimeoutSeconds: 240, MaxSessionTokens: 1}},
		Messages:      []session.TextMessage{{Text: "Use exec_command to run sleep 60 with yield_time_ms=1000. Keep polling the command until it exits. Do not finish the task before the command exits. Then reply DONE."}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		record, _, err := s.Read(cleanup, a.SessionID)
		if err != nil {
			t.Error(err)
			return
		}
		if record.SandboxID != nil {
			if _, err := client.Sandboxes.Kill(cleanup, *record.SandboxID); err != nil {
				t.Error("sandbox cleanup failed")
			}
		}
	})
	e := NewExecutor(a.SessionID, s, platform)
	defer e.Disconnect()
	for ctx.Err() == nil {
		if err := e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		run, err := s.Run(ctx, a.SessionID, a.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status.Terminal() {
			if run.Status != session.Cancelled || run.StopReason == nil || *run.StopReason != "token_limit" || run.StopMethod == nil || run.Usage.TotalTokens <= 0 || run.Usage.InputTokens <= 0 || run.Usage.OutputTokens <= 0 {
				t.Fatalf("unexpected token budget outcome: status=%s reason=%v method=%v usage=%+v error=%v", run.Status, run.StopReason, run.StopMethod, run.Usage, run.Error)
			}
			record, _, err := s.Read(ctx, a.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if record.Usage() != run.Usage || record.SandboxState == "unavailable" {
				t.Fatal("session usage or environment differs", record.Usage(), record.SandboxState)
			}
			if !record.SlotReserved {
				if record.SandboxState != "paused" {
					t.Fatal("sandbox was not paused", record.SandboxState)
				}
				_, err := s.Accept(ctx, store.Admission{SessionID: a.SessionID, Key: uuid.New(), Messages: []session.TextMessage{{Text: "Continue"}}})
				if apiError, ok := errors.AsType[*session.APIError](err); !ok || apiError.Problem.Code != "token_limit_exceeded" {
					t.Fatal("exhausted session accepted work", err)
				}
				t.Logf("stream usage: %+v; stop_method=%s; sandbox paused", run.Usage, *run.StopMethod)
				return
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	t.Fatal("live token budget run timed out")
}
