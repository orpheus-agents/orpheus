//go:build live

package worker

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/agentbox"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/secret"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

// Exercise automatic cleanup of real running and paused sandboxes without a model call.
func TestLiveSingleRunSandboxDeletion(t *testing.T) {
	if os.Getenv("AGENTBOX_API_KEY") == "" {
		t.Fatal("AGENTBOX_API_KEY is required")
	}
	for _, paused := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "paused"}[paused], func(t *testing.T) {
			platform, err := agentbox.New()
			if err != nil {
				t.Fatal(err)
			}
			cipher, err := secret.New(base64.URLEncoding.EncodeToString(make([]byte, 32)))
			if err != nil {
				t.Fatal(err)
			}
			s := &store.Store{Pool: testutil.Database(t), Settings: config.DefaultSettings(), Cipher: cipher, Profiles: config.Profiles{Profiles: map[string]config.Profile{"live": {Harness: "codex", Model: new("fixture"), Auth: config.Auth{Mode: "api_key", APIKeyEnv: "OPENAI_API_KEY"}}}}}
			s.Settings.WorkerPoll = 50 * time.Millisecond
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			a, err := s.Accept(ctx, store.Admission{Key: uuid.New(), Create: &session.CreateSession{Configuration: session.ConfigurationInput{
				Agent: session.AgentInput{Profile: "live"}, Sandbox: session.SandboxInput{Template: "codex"}, Limits: session.Limits{RunTimeoutSeconds: 60},
			}, Messages: []session.TextMessage{{Text: "single-run sandbox cleanup test"}}}})
			if err != nil {
				t.Fatal(err)
			}
			e := NewExecutor(a.SessionID, s, platform)
			defer e.Disconnect()
			record, run, err := s.Read(ctx, a.SessionID)
			if err != nil || run == nil {
				t.Fatal(err)
			}
			if err := e.ensureSandbox(ctx, &record, run); err != nil {
				t.Fatal(err)
			}
			id := *record.SandboxID
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := platform.Delete(cleanup, id); err != nil && !errors.Is(err, harness.ErrNotFound) {
					t.Error(err)
				}
			})
			if err := e.preparePaths(ctx, &record); err != nil {
				t.Fatal(err)
			}
			if paused {
				if err := e.sandbox.Pause(ctx); err != nil {
					t.Fatal(err)
				}
			}
			e.Disconnect() // Also exercise recovery after a worker restart.
			if _, err := s.Cancel(ctx, a.SessionID, a.RunID); err != nil {
				t.Fatal(err)
			}
			e.Run(ctx)
			if ctx.Err() != nil {
				t.Fatal(ctx.Err())
			}
			view, err := s.Session(ctx, a.SessionID)
			if err != nil || view.AllowMultipleRuns || view.Status != session.Cancelled || view.Sandbox.State != "deleted" || view.Sandbox.ID == nil || *view.Sandbox.ID != id || view.Sandbox.Workspace == nil {
				t.Fatal(view, err)
			}
			if _, err := platform.Info(ctx, id); !errors.Is(err, harness.ErrNotFound) {
				t.Fatal("deleted sandbox is still present", err)
			}
			found, err := platform.Find(ctx, map[string]string{"orpheus_session_id": a.SessionID.String()})
			if err != nil || len(found) != 0 {
				t.Fatal("deleted sandbox is still listed", found, err)
			}
			if _, err := platform.Connect(ctx, id, time.Minute); !errors.Is(err, harness.ErrNotFound) {
				t.Fatal("deleted sandbox can be resumed", err)
			}
		})
	}
}
