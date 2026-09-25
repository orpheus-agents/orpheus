//go:build live

package codex

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	sdk "github.com/abox-dev/sdk/packages/go-sdk"
	"github.com/orpheus-agents/orpheus/internal/agentbox"
	"github.com/orpheus-agents/orpheus/internal/credentials"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/session"
)

// Run explicitly with a test ChatGPT auth file. The file is copied to a
// short-lived sandbox and never written to test output or committed fixtures.
func TestLiveAccountLimits(t *testing.T) {
	path := os.Getenv("ORPHEUS_TEST_ACCOUNT_AUTH_FILE")
	if path == "" {
		t.Skip("ORPHEUS_TEST_ACCOUNT_AUTH_FILE is required for live account limits")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read test account credentials")
	}
	defer clear(raw)
	if err := credentials.Validate(raw); err != nil {
		t.Fatal("invalid test account credentials")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	platform, err := agentbox.New()
	if err != nil {
		t.Fatal(err)
	}
	box, err := platform.Create(ctx, "codex", 180*time.Second, map[string]string{"purpose": "orpheus-account-limits-live-test"})
	if err != nil {
		t.Fatal(err)
	}
	client, err := sdk.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := client.Sandboxes.Kill(cleanup, box.ID()); err != nil {
			t.Error("sandbox cleanup failed")
		}
	})
	version, err := box.Run(ctx, "codex --version")
	if err != nil || !strings.HasPrefix(string(version), "codex-cli ") {
		t.Fatal("sandbox Codex version unavailable", err)
	}
	t.Log("sandbox", strings.TrimSpace(string(version)))
	driver := New(box, 30*time.Second, 524288)
	defer func() { _ = driver.Close() }()
	home, err := driver.StateDir(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := box.Write(ctx, home+"/auth.json", raw); err != nil {
		t.Fatal("cannot seed test credentials")
	}
	if _, err := box.Run(ctx, "chmod 600 "+harness.Quote(home+"/auth.json")); err != nil {
		t.Fatal("cannot protect test credentials")
	}
	copyReader, err := box.Read(ctx, home+"/auth.json")
	if err != nil {
		t.Fatal("cannot read seeded credentials")
	}
	seeded, readErr := io.ReadAll(copyReader)
	closeErr := copyReader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(raw, seeded) {
		clear(seeded)
		t.Fatal("seeded credentials differ from local file", readErr, closeErr)
	}
	clear(seeded)
	if _, err := box.Run(ctx, `codex -c 'cli_auth_credentials_store="file"' -c 'forced_login_method="chatgpt"' login status >/dev/null 2>&1`); err != nil {
		t.Fatal("sandbox does not recognize seeded ChatGPT login")
	}
	source := session.Credentials{Mode: "account"}
	if _, err := driver.Launch(ctx, nil, home, source); err != nil {
		t.Fatal(err)
	}
	if err := driver.Initialize(ctx, source, true); err != nil {
		if rpcError, ok := errors.AsType[*RPCError](err); ok {
			reason := "provider_rejected"
			if rpcError.NativeMessage == "workspace routing discovery failed" {
				reason = "workspace_discovery_failed"
			}
			t.Fatalf("account initialization RPC code %d (%s)", rpcError.Code, reason)
		}
		t.Fatal(err)
	}
	readCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	snapshot, err := driver.ReadAccountLimits(readCtx)
	if err != nil {
		t.Fatal("rate-limit read failed", err)
	}
	if len(snapshot.Buckets) == 0 {
		t.Fatal("account returned no limit buckets")
	}
	for _, bucket := range snapshot.Buckets {
		if bucket.LimitID == "" {
			t.Fatal("empty limit ID")
		}
	}
}
