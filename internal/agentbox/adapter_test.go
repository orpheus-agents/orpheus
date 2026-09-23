package agentbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdk "github.com/abox-dev/sdk/packages/go-sdk"
	"github.com/orpheus-agents/orpheus/internal/harness"
)

func TestSandboxOperationsSelectUser(t *testing.T) {
	requests := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sandboxes/sbx/connect" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"sandboxID":"sbx","templateID":"codex","envdVersion":"0.6.4"}`)
			return
		}
		requests <- r.Method + " " + r.URL.Path
		if r.URL.Path == "/files" {
			if got := r.URL.Query().Get("username"); got != "user" {
				t.Errorf("file username = %q, want user", got)
			}
			if got := r.URL.Query().Get("path"); got != "~/auth.json" {
				t.Errorf("file path = %q", got)
			}
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, "fixture")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[{"name":"auth.json","path":"/home/user/auth.json","type":"file"}]`)
			return
		}
		user, password, ok := r.BasicAuth()
		if !ok || user != "user" || password != "" {
			t.Errorf("%s: expected explicit user identity", r.URL.Path)
		}
		// Reject the request after recording identity. None of these operations
		// should retry as root or as the template's default user.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":"unauthenticated","message":"unknown user"}`)
	}))
	defer server.Close()
	client, err := sdk.NewClient(sdk.WithAPIKey("fixture"), sdk.WithAPIURL(server.URL), sdk.WithSandboxURL(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	p := &Platform{client: client}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	box, err := p.Connect(ctx, "sbx", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	nextRequest := func(t *testing.T) string {
		t.Helper()
		select {
		case request := <-requests:
			return request
		case <-ctx.Done():
			t.Fatal("expected SDK request")
			return ""
		}
	}
	for _, tc := range []struct {
		name, path string
		call       func() error
	}{
		{"run", "/process.Process/Start", func() error { _, err := box.Run(ctx, "id -un"); return err }},
		{"start", "/process.Process/Start", func() error { _, _, err := box.Start(ctx, "exec cat", nil, ""); return err }},
		{"watch", "/filesystem.Filesystem/WatchDir", func() error {
			w, err := box.Watch(ctx, "~")
			if err != nil {
				return err
			}
			select {
			case <-w.Done():
			case <-ctx.Done():
			}
			return w.Close()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); err == nil {
				t.Fatal("expected rejected request")
			}
			if got := nextRequest(t); got != "POST "+tc.path {
				t.Fatalf("unexpected request: %s", got)
			}
		})
	}
	reader, err := box.Read(ctx, "~/auth.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "fixture" {
		t.Fatalf("read: %q, %v", data, err)
	}
	if got := nextRequest(t); got != "GET /files" {
		t.Fatal(got)
	}
	if err := box.Write(ctx, "~/auth.json", []byte("fixture")); err != nil {
		t.Fatal(err)
	}
	if got := nextRequest(t); got != "POST /files" {
		t.Fatal(got)
	}
	if len(requests) != 0 {
		t.Fatal("unexpected retries")
	}
}

func TestStreamSetupDeadline(t *testing.T) {
	called := 0
	_, _, err := openStream(t.Context(), 10*time.Millisecond, func(ctx context.Context) (*sdk.CommandHandle, error) { called++; <-ctx.Done(); return nil, ctx.Err() })
	if err == nil || called != 1 {
		t.Fatalf("setup did not time out once: %d %v", called, err)
	}
}

func TestPIDDeadlineDoesNotLimitConnectedStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sandboxes/sbx/connect" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"sandboxID":"sbx","templateID":"codex","envdVersion":"0.6.4"}`)
			return
		}
		w.Header().Set("Content-Type", "application/connect+proto")
		w.WriteHeader(200)
		if err := http.NewResponseController(w).Flush(); err != nil {
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := sdk.NewClient(sdk.WithAPIKey("fixture"), sdk.WithAPIURL(server.URL), sdk.WithSandboxURL(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	box, err := client.Sandboxes.Connect(t.Context(), "sbx", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The server accepts Start but never supplies the PID acknowledgement.
	_, _, err = openStream(t.Context(), 20*time.Millisecond, func(ctx context.Context) (*sdk.CommandHandle, error) { return box.Commands.Start(ctx, "cat", nil) })
	if err == nil {
		t.Fatal("missing PID did not time out")
	}
	var streamCtx context.Context
	s, pid, err := openStream(t.Context(), 100*time.Millisecond, func(ctx context.Context) (*sdk.CommandHandle, error) {
		streamCtx = ctx
		return box.Commands.ConnectWithOptions(ctx, 123, "", &sdk.CommandConnectOptions{Streaming: &sdk.CommandStreamingOptions{StdoutChannel: true}})
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	<-time.After(150 * time.Millisecond)
	if pid != 123 || streamCtx.Err() != nil {
		t.Fatalf("setup deadline cancelled connected stream: %d %v", pid, streamCtx.Err())
	}
}

func TestSDKRejectionClassification(t *testing.T) {
	for _, err := range []error{&sdk.AuthenticationError{}, &sdk.InvalidArgumentError{}, &sdk.FileNotFoundError{}, &sdk.CommandExitError{}, &sdk.NotEnoughSpaceError{}, &sdk.TemplateError{}, &sdk.SandboxError{StatusCode: 403, Message: "private"}} {
		t.Run(fmt.Sprintf("%T", err), func(t *testing.T) {
			got := classify(fmt.Errorf("private: %w", err))
			if !errors.Is(got, harness.ErrEnvironmentRejected) || !errors.Is(got, harness.ErrRejected) || errors.Is(got, harness.ErrNotFound) {
				t.Fatal(got)
			}
		})
	}
	if !errors.Is(classify(&sdk.SandboxNotFoundError{}), harness.ErrNotFound) {
		t.Fatal("sandbox loss not recognized")
	}
	for _, code := range []int{408, 429, 500, 503} {
		original := &sdk.SandboxError{StatusCode: code}
		if got := classify(original); !errors.Is(got, original) {
			t.Fatal("uncertain operation classified as rejected", code)
		}
	}
}

func TestMissingFileReadIsNotEnvironmentRejection(t *testing.T) {
	for _, tc := range []struct {
		code int
		want error
	}{
		{http.StatusNotFound, fs.ErrNotExist},
		{http.StatusForbidden, harness.ErrEnvironmentRejected},
	} {
		t.Run(fmt.Sprint(tc.code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/sandboxes/sbx/connect" {
					_, _ = io.WriteString(w, `{"sandboxID":"sbx","templateID":"codex","envdVersion":"0.6.15"}`)
					return
				}
				if r.URL.Path != "/files" || r.Method != http.MethodGet {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(tc.code)
				_, _ = io.WriteString(w, `{"message":"file unavailable"}`)
			}))
			defer server.Close()
			client, err := sdk.NewClient(sdk.WithAPIKey("fixture"), sdk.WithAPIURL(server.URL), sdk.WithSandboxURL(server.URL))
			if err != nil {
				t.Fatal(err)
			}
			p := &Platform{client: client}
			box, err := p.Connect(t.Context(), "sbx", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			reader, err := box.Read(t.Context(), "/home/user/.orpheus/hooks/operation/result.json")
			if reader != nil {
				_ = reader.Close()
				t.Fatal("unexpected reader")
			}
			if !errors.Is(err, tc.want) || errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("read error = %v, want %v without sandbox loss", err, tc.want)
			}
		})
	}
}
