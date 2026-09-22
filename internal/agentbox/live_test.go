//go:build live

package agentbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	sdk "github.com/abox-dev/sdk/packages/go-sdk"

	"github.com/skillum-ai/orpheus/internal/harness"
)

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
