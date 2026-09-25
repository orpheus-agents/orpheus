//go:build live

package worker

import (
	"context"
	"encoding/base64"
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

// Exercise the worker's sandbox requests with the default Mattermost deadlines
// against real AgentBox, without invoking a model or sending chat messages.
func TestLiveSandboxLeaseLifecycle(t *testing.T) {
	if os.Getenv("AGENTBOX_API_KEY") == "" {
		t.Fatal("AGENTBOX_API_KEY is required")
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
	s := &store.Store{Pool: testutil.Database(t), Settings: config.DefaultSettings(), Cipher: cipher, Profiles: config.Profiles{Profiles: map[string]config.Profile{"live": {Harness: "codex", Model: new("gpt-6-sol"), Auth: config.Auth{Mode: "api_key", APIKeyEnv: "OPENAI_API_KEY"}}}}}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	a, err := s.Accept(ctx, store.Admission{Key: uuid.New(), Create: &session.CreateSession{Configuration: session.ConfigurationInput{
		Agent: session.AgentInput{Profile: "live"}, Sandbox: session.SandboxInput{Template: "codex"},
		Limits: session.Limits{RunTimeoutSeconds: 3600}, Hooks: &session.HooksInput{TimeoutSeconds: new(120)},
	}, Messages: []session.TextMessage{{Text: "sandbox lease lifecycle"}}}})
	if err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(a.SessionID, s, platform)
	defer e.Disconnect()
	record, run, err := s.Read(ctx, a.SessionID)
	if err != nil || run == nil {
		t.Fatal("read pending run", err)
	}
	if err := e.ensureSandbox(ctx, &record, run); err != nil {
		t.Fatal("create", err)
	}
	id := *record.SandboxID
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := client.Sandboxes.Kill(cleanup, id); err != nil {
			t.Error("sandbox cleanup failed")
		}
	})
	e.timeoutRenewAt = time.Time{}
	if err := e.ensureSandbox(ctx, &record, run); err != nil {
		t.Fatal("renew", err)
	}
	e.Disconnect()
	if err := e.ensureSandbox(ctx, &record, run); err != nil {
		t.Fatal("reconnect", err)
	}
	if err := e.sandbox.Pause(ctx); err != nil {
		t.Fatal("pause", err)
	}
	e.Disconnect()
	if err := e.ensureSandbox(ctx, &record, run); err != nil {
		t.Fatal("resume", err)
	}
	if e.sandbox.ID() != id {
		t.Fatal("resume replaced the sandbox")
	}
	if state, err := platform.Info(ctx, id); err != nil || state != "running" {
		t.Fatal("sandbox did not resume", state, err)
	}
}
