//go:build live

package agentbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	sdk "github.com/abox-dev/sdk/packages/go-sdk"

	"github.com/orpheus-agents/orpheus/internal/harness"
)

func TestLiveSandboxAccessSDK(t *testing.T) {
	if os.Getenv("AGENTBOX_API_KEY") == "" {
		t.Fatal("AGENTBOX_API_KEY is required")
	}
	platform, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	box, err := platform.Create(ctx, "codex", 3*time.Minute, map[string]string{"purpose": "orpheus-sandbox-access-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := platform.client.Sandboxes.Kill(cleanup, box.ID()); err != nil {
			t.Error("sandbox cleanup failed")
		}
	})
	workspace, err := box.Run(ctx, `mkdir -p "$HOME/workspace"; printf '%s' "$HOME/workspace"`)
	if err != nil {
		t.Fatal(err)
	}
	if reader, err := box.Read(ctx, path.Join(string(workspace), "not-yet-created-hook-result.json")); !errors.Is(err, fs.ErrNotExist) {
		if reader != nil {
			_ = reader.Close()
		}
		t.Fatalf("missing hook result must remain a file absence: %v", err)
	}
	if err := box.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	// An integration uses its own SDK client and only the public sandbox address.
	client, err := sdk.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	external, err := client.Sandboxes.Connect(ctx, box.ID(), nil)
	if err != nil {
		t.Fatal(err)
	}
	file := path.Join(string(workspace), "plugin-probe.txt")
	if _, err := external.Files.WriteBytes(ctx, file, []byte("from-plugin"), &sdk.WriteFileOptions{User: "user"}); err != nil {
		t.Fatal(err)
	}
	reader, err := external.Files.Read(ctx, file, &sdk.FileOptions{User: "user"})
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(content) != "from-plugin" {
		t.Fatal(string(content), err)
	}
	if err := external.Pause(ctx, &sdk.PauseOptions{Memory: new(true)}); err != nil {
		t.Fatal(err)
	}
	resumed, err := platform.Connect(ctx, box.ID(), 3*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	reader, err = resumed.Read(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	content, err = io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(content) != "from-plugin" {
		t.Fatal("file lost across plugin pause", string(content), err)
	}
}

func TestLiveStreamingDetachReconnect(t *testing.T) {
	if os.Getenv("AGENTBOX_API_KEY") == "" {
		t.Fatal("AGENTBOX_API_KEY is required")
	}
	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	box, err := p.Create(ctx, "codex", 180*time.Second, map[string]string{"purpose": "orpheus-go-stream-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := p.client.Sandboxes.Kill(cleanup, box.ID()); err != nil {
			t.Error("sandbox cleanup failed")
		}
	})
	identity, err := box.Run(ctx, "id -un")
	if err != nil || strings.TrimSpace(string(identity)) != "user" {
		t.Fatalf("command user = %q, error = %v", identity, err)
	}
	process, pid, err := box.Start(ctx, "id -un; exec cat", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	readUntil := func(s harness.Stream, text string) {
		t.Helper()
		var raw []byte
		for !bytes.Contains(raw, []byte(text)) {
			select {
			case chunk, ok := <-s.Stdout():
				if !ok {
					t.Fatal("stdout closed")
				}
				raw = append(raw, chunk...)
			case <-ctx.Done():
				t.Fatal("round trip timed out")
			}
		}
	}
	readUntil(process, "user\n")
	roundTrip := func(s harness.Stream, text string) {
		t.Helper()
		if _, err := s.Write(ctx, []byte(text)); err != nil {
			t.Fatal(err)
		}
		readUntil(s, text)
	}
	roundTrip(process, "first\n")
	if err := process.Close(); err != nil {
		t.Fatal(err)
	}
	attached, err := box.Attach(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = attached.Close() }()
	roundTrip(attached, "second\n")
	// Verify the published SDK's streaming result doesn't retain even the output
	// already consumed by Orpheus; detach itself must not terminate the process.
	if err := box.Kill(ctx, pid); err != nil {
		t.Fatal(err)
	}
	result, err := attached.(*stream).h.Wait(ctx)
	if err != nil {
		if _, ok := errors.AsType[*sdk.CommandExitError](err); !ok {
			t.Fatal(err)
		}
	}
	if len(result.Stdout) != 0 || len(result.Stderr) != 0 {
		t.Fatal("streaming attachment retained output")
	}
}
