package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/session"
)

type testStream struct {
	out, errout chan []byte
	write       func([]byte)
	writeErr    error
}

func (s *testStream) Stdout() <-chan []byte { return s.out }
func (s *testStream) Stderr() <-chan []byte { return s.errout }
func (s *testStream) Write(_ context.Context, b []byte) (int, error) {
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	s.write(b)
	return len(b), nil
}
func (s *testStream) Close() error { return nil }

type testBox struct {
	harness.Sandbox
	stream       *testStream
	startCommand string
	startEnv     map[string]string
	startCWD     string
	run          func(context.Context, string) ([]byte, error)
	files        map[string][]byte
}

func (b *testBox) Start(_ context.Context, command string, env map[string]string, cwd string) (harness.Stream, int, error) {
	b.startCommand = command
	b.startEnv = maps.Clone(env)
	b.startCWD = cwd
	return b.stream, 123, nil
}
func (b *testBox) Attach(context.Context, int) (harness.Stream, error) { return b.stream, nil }
func (b *testBox) Run(ctx context.Context, command string) ([]byte, error) {
	if b.run != nil {
		return b.run(ctx, command)
	}
	return exec.CommandContext(ctx, "sh", "-c", command).Output()
}
func (b *testBox) Write(_ context.Context, path string, raw []byte) error {
	if b.files == nil {
		b.files = map[string][]byte{}
	}
	b.files[path] = raw
	return nil
}
func newRPC(t *testing.T, respond func(map[string]json.RawMessage) any) (*RPC, *testBox) {
	t.Helper()
	stream := &testStream{out: make(chan []byte, 64), errout: make(chan []byte)}
	stream.write = func(raw []byte) {
		var req map[string]json.RawMessage
		_ = json.Unmarshal(raw, &req)
		reply := respond(req)
		if reply == nil {
			return
		}
		data, _ := json.Marshal(reply)
		data = append(data, '\n')
		stream.out <- data[:min(5, len(data))]
		stream.out <- data[min(5, len(data)):]
	}
	box := &testBox{stream: stream}
	rpc := NewRPC(box, 100*time.Millisecond)
	if _, err := rpc.Launch(t.Context(), "exec codex app-server", nil, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rpc.Close() })
	return rpc, box
}
func TestRPCFragmentationRejectionAndNoRetry(t *testing.T) {
	calls := 0
	rpc, _ := newRPC(t, func(req map[string]json.RawMessage) any {
		calls++
		var method string
		_ = json.Unmarshal(req["method"], &method)
		if method == "lost" {
			return nil
		}
		if method == "error" {
			return map[string]any{"id": req["id"], "error": map[string]any{"code": 1, "message": "private-secret"}}
		}
		return map[string]any{"id": req["id"], "result": map[string]string{"text": "я"}}
	})
	var out struct{ Text string }
	if err := rpc.Call(t.Context(), "read", nil, &out); err != nil || out.Text != "я" {
		t.Fatal(out, err)
	}
	if err := rpc.Call(t.Context(), "error", nil, nil); !errors.Is(err, harness.ErrRejected) || strings.Contains(err.Error(), "private") {
		t.Fatal(err)
	}
	if err := rpc.Call(t.Context(), "lost", nil, nil); !errors.Is(err, harness.ErrUncertain) {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatal("RPC retried")
	}
}

func TestStartInjectsEarlierMessagesSeparately(t *testing.T) {
	var calls []map[string]json.RawMessage
	rpc, _ := newRPC(t, func(req map[string]json.RawMessage) any {
		calls = append(calls, req)
		var method string
		_ = json.Unmarshal(req["method"], &method)
		if method == "turn/start" {
			return map[string]any{"id": req["id"], "result": map[string]any{"turn": map[string]string{"id": "turn-1"}}}
		}
		return map[string]any{"id": req["id"], "result": map[string]any{}}
	})
	driver := &Driver{rpc: rpc}
	turn, err := driver.Start(t.Context(), session.AgentConfiguration{}, "thread-1", []string{"first", "second", "third"})
	if err != nil || turn != "turn-1" || len(calls) != 2 {
		t.Fatalf("start calls: %s %v %+v", turn, err, calls)
	}
	var injected struct {
		Items []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"items"`
	}
	if err := json.Unmarshal(calls[0]["params"], &injected); err != nil {
		t.Fatal(err)
	}
	if len(injected.Items) != 2 || injected.Items[0].Role != "user" || injected.Items[0].Content[0].Text != "first" || injected.Items[1].Content[0].Text != "second" {
		t.Fatalf("injected items: %+v", injected.Items)
	}
	var started struct {
		Input []struct {
			Text string `json:"text"`
		} `json:"input"`
	}
	if err := json.Unmarshal(calls[1]["params"], &started); err != nil {
		t.Fatal(err)
	}
	if len(started.Input) != 1 || started.Input[0].Text != "third" {
		t.Fatalf("turn input: %+v", started.Input)
	}
}

func TestRejectedBatchedStartRetiresInjectedContext(t *testing.T) {
	rpc, _ := newRPC(t, func(req map[string]json.RawMessage) any {
		var method string
		_ = json.Unmarshal(req["method"], &method)
		if method == "turn/start" {
			return map[string]any{"id": req["id"], "error": map[string]any{"code": 1, "message": "rejected"}}
		}
		return map[string]any{"id": req["id"], "result": map[string]any{}}
	})
	driver := &Driver{rpc: rpc}
	_, err := driver.Start(t.Context(), session.AgentConfiguration{}, "thread-1", []string{"first", "second"})
	failure, ok := errors.AsType[*harness.ExecutionError](err)
	if !ok || failure.Code != "context_lost" {
		t.Fatalf("rejected injected context remained reusable: %v", err)
	}
}
func TestRPCConcurrentCallsAndApproval(t *testing.T) {
	rpc, box := newRPC(t, func(req map[string]json.RawMessage) any {
		return map[string]any{"id": req["id"], "result": map[string]bool{"ok": true}}
	})
	var wg sync.WaitGroup
	for range 30 {
		wg.Go(func() {
			var out struct {
				OK bool `json:"ok"`
			}
			if err := rpc.Call(t.Context(), "read", nil, &out); err != nil || !out.OK {
				t.Errorf("%v %v", out, err)
			}
		})
	}
	wg.Wait()
	box.stream.out <- []byte("garbage\n")
	deadline := time.Now().Add(time.Second)
	for {
		_, dirty := rpc.updates()
		if dirty {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("malformed output did not mark history dirty")
		}
		time.Sleep(time.Millisecond)
	}
	// Exercise the server-request response without exposing its private payload.
	var response map[string]json.RawMessage
	box.stream.write = func(raw []byte) { _ = json.Unmarshal(raw, &response) }
	rpc.receive(t.Context(), []byte(`{"id":1,"method":"approval","params":{"secret":"x"}}`))
	if len(response["error"]) == 0 {
		t.Fatal("approval was not rejected")
	}
}
func TestBoundedResults(t *testing.T) {
	for _, v := range []any{strings.Repeat("я", 1000), map[string]string{"stdout": strings.Repeat("x", 1000), "stderr": strings.Repeat("y", 1000)}} {
		raw, _ := json.Marshal(v)
		out := Bounded(raw, 101, new(0), false)
		var r Result
		_ = json.Unmarshal(out.Result, &r)
		if r.Type != "truncated_text" || len(*r.Head)+len(*r.Tail) > 101 || !utf8.ValidString(*r.Head) || !utf8.ValidString(*r.Tail) || r.OriginalBytes == nil {
			t.Fatal(r)
		}
	}
	out := Bounded(json.RawMessage(`"tail"`), 100, nil, true)
	var r Result
	_ = json.Unmarshal(out.Result, &r)
	if r.OriginalBytes != nil || out.Reason == nil || *out.Reason != "harness_limit" {
		t.Fatal(out)
	}
}
func TestCapturedLiveAndOfflineParity(t *testing.T) {
	for _, name := range []string{"codex-native-history.json", "codex-streamed-output.json", "codex-file-change.json"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("../../../tests/fixtures", name))
			if err != nil {
				t.Fatal(err)
			}
			var pair struct {
				Thread  Thread            `json:"thread"`
				Records []json.RawMessage `json:"records"`
			}
			if err := json.Unmarshal(raw, &pair); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "history.jsonl")
			var lines []byte
			for _, r := range pair.Records {
				var compact bytes.Buffer
				if err := json.Compact(&compact, r); err != nil {
					t.Fatal(err)
				}
				lines = append(lines, compact.Bytes()...)
				lines = append(lines, '\n')
			}
			if err := os.WriteFile(path, append(lines, []byte(`{"incomplete"`)...), 0600); err != nil {
				t.Fatal(err)
			}
			box := &testBox{}
			var journal journalBatch
			if err := readNative(t.Context(), box, "journal", path, 0, 524288, &journal); err != nil {
				t.Fatal(err)
			}
			if journal.Offset != int64(len(lines)) {
				t.Fatal("partial line consumed")
			}
			completed := map[string]bool{}
			outputs := map[string]Output{}
			orders := map[string]int64{}
			commands := map[string]json.RawMessage{}
			for _, item := range journal.Items {
				switch item.Type {
				case "completed":
					completed[item.ID] = true
				case "output":
					outputs[item.ID] = item.Output
				case "order":
					orders[item.ID] = item.StartedAtMS
				case "command":
					commands[item.ID] = item.Argv
				}
			}
			live := Normalize(pair.Thread, completed, outputs, 524288, orders, commands)
			offline, err := recoverHistory(t.Context(), box, nil, &path, nil, 524288)
			if err != nil {
				t.Fatal(err)
			}
			offline.Path = nil
			offline.Offset = 0
			if !reflect.DeepEqual(live, offline) {
				lb, _ := json.MarshalIndent(live, "", "  ")
				ob, _ := json.MarshalIndent(offline, "", "  ")
				t.Fatalf("live/offline mismatch\n%s\n%s", lb, ob)
			}
			for _, turn := range live.Turns {
				for _, item := range turn.Items {
					if item.Type == "tool" && item.Completeness != "complete" {
						t.Fatalf("lost full tool output: %s", item.NativeID)
					}
				}
			}
		})
	}
}
func TestNativeArgvAndInterruptedTool(t *testing.T) {
	input := json.RawMessage(`"bash -c 'unterminated"`)
	thread := Thread{Turns: []NativeTurn{{ID: "t", Status: "interrupted", Items: []NativeItem{{ID: "tool", Type: "commandExecution", Command: input, Status: "inProgress"}}}}}
	snapshot := Normalize(thread, nil, nil, 100, nil, nil)
	if snapshot.Turns[0].Status != session.Cancelled || snapshot.Turns[0].Items[0].Status != "unknown" {
		t.Fatal(snapshot)
	}
	argv := json.RawMessage(`["bash","-c","echo $HOME"]`)
	snapshot = Normalize(thread, nil, nil, 100, nil, map[string]json.RawMessage{"tool": argv})
	if string(snapshot.Turns[0].Items[0].Input) != string(argv) {
		t.Fatal(snapshot)
	}
}

func TestPlatformDeliveryRejectionIsDefinitive(t *testing.T) {
	rpc, box := newRPC(t, func(map[string]json.RawMessage) any { t.Fatal("rejected write reached native server"); return nil })
	box.stream.writeErr = harness.ErrRejected
	if err := rpc.Call(t.Context(), "turn/start", nil, nil); !errors.Is(err, harness.ErrRejected) {
		t.Fatal("definitive delivery rejection became uncertain", err)
	}
}
