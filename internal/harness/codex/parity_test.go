package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"time"

	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/session"
)

// Expected values were captured by executing the original Python main. They
// deliberately include edge cases absent from the original Python test suite.
func TestPythonProjectionParity(t *testing.T) {
	raw, err := os.ReadFile("../../../tests/fixtures/python-parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Commands []struct {
			Display  string
			Expected json.RawMessage
		}
		Bounded []struct {
			Value      json.RawMessage
			Limit      int
			Incomplete bool
			Expected   Output
		}
		Normalization []struct {
			Thread    Thread
			Completed []string
			Orders    map[string]int64
			Expected  json.RawMessage
		}
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	equal := func(t *testing.T, got, want json.RawMessage) {
		t.Helper()
		var a, b any
		for _, v := range []struct {
			raw json.RawMessage
			dst *any
		}{{got, &a}, {want, &b}} {
			d := json.NewDecoder(bytes.NewReader(v.raw))
			d.UseNumber()
			if err := d.Decode(v.dst); err != nil {
				t.Fatal(err)
			}
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("got %s\nwant %s", got, want)
		}
	}
	for i, tc := range fixture.Commands {
		t.Run(fmt.Sprintf("display-%d", i), func(t *testing.T) {
			command, _ := json.Marshal(tc.Display)
			thread := Thread{Turns: []NativeTurn{{ID: "t", Status: "completed", Items: []NativeItem{{ID: "i", Type: "commandExecution", Command: command}}}}}
			got := Normalize(thread, nil, nil, 1024, nil, nil)
			equal(t, got.Turns[0].Items[0].Input, tc.Expected)
		})
	}
	for i, tc := range fixture.Bounded {
		t.Run(fmt.Sprintf("bounded-%d", i), func(t *testing.T) {
			got := Bounded(tc.Value, tc.Limit, nil, tc.Incomplete)
			equal(t, got.Result, tc.Expected.Result)
			if got.Completeness != tc.Expected.Completeness || !reflect.DeepEqual(got.Reason, tc.Expected.Reason) {
				t.Fatal(got, tc.Expected)
			}
		})
	}
	for _, tc := range fixture.Normalization {
		t.Run(tc.Thread.Turns[0].Status, func(t *testing.T) {
			completed := map[string]bool{}
			for _, id := range tc.Completed {
				completed[id] = true
			}
			got, _ := json.Marshal(Normalize(tc.Thread, completed, nil, 1024, tc.Orders, nil))
			var expected harness.Snapshot
			if err := json.Unmarshal(tc.Expected, &expected); err != nil {
				t.Fatal(err)
			}
			want, _ := json.Marshal(expected)
			equal(t, got, want)
		})
	}
}

func TestExplicitNullToolInputIsPreserved(t *testing.T) {
	thread := Thread{Turns: []NativeTurn{{ID: "turn", Status: "completed", Items: []NativeItem{{ID: "tool", Type: "commandExecution", Arguments: json.RawMessage("null"), Command: json.RawMessage(`"fallback"`)}}}}}
	got := Normalize(thread, nil, nil, 1024, nil, nil)
	if string(got.Turns[0].Items[0].Input) != "null" {
		t.Fatal("null arguments were replaced", got.Turns[0].Items[0])
	}
}

func TestNativePreparationAndAccountModes(t *testing.T) {
	for _, mode := range []string{"api_key", "account"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("FIXTURE_KEY", "private-key")
			logins, reads := 0, 0
			accountType := "chatgpt"
			rpc, box := newRPC(t, func(req map[string]json.RawMessage) any {
				var method string
				_ = json.Unmarshal(req["method"], &method)
				if method == "initialized" {
					return nil
				}
				if method == "account/login/start" {
					logins++
				}
				var result any = map[string]any{}
				if method == "account/read" {
					reads++
					result = map[string]any{"account": map[string]string{"type": accountType}}
				}
				return map[string]any{"id": req["id"], "result": result}
			})
			d := New(box, time.Second, 1024)
			d.rpc = rpc
			source := session.Credentials{Mode: mode, APIKeyEnv: "FIXTURE_KEY"}
			box.run = func(_ context.Context, command string) ([]byte, error) {
				if !strings.Contains(command, "${CODEX_HOME:-$HOME/.codex}") {
					t.Fatal("Codex state directory did not follow image environment", command)
				}
				return []byte("/home/user/.codex"), nil
			}
			home, err := d.StateDir(t.Context())
			if err != nil || home != "/home/user/.codex" || len(box.files) != 0 {
				t.Fatal(home, box.files, err)
			}
			launchBox := &testBox{stream: &testStream{out: make(chan []byte), errout: make(chan []byte)}}
			launcher := New(launchBox, time.Second, 1024)
			defer func() { _ = launcher.Close() }()
			if _, err := launcher.Launch(t.Context(), map[string]string{"TOKEN": "value"}, "/workspace", source); err != nil {
				t.Fatal(err)
			}
			wantMode := "api"
			if mode == "account" {
				wantMode = "chatgpt"
			}
			for _, option := range []string{
				`cli_auth_credentials_store="file"`,
				`forced_login_method="` + wantMode + `"`,
				`approval_policy="never"`,
				`sandbox_mode="danger-full-access"`,
			} {
				if !strings.Contains(launchBox.startCommand, " -c "+harness.Quote(option)) {
					t.Fatal("missing Codex launch option", option, launchBox.startCommand)
				}
			}
			if !strings.HasSuffix(launchBox.startCommand, " app-server") || launchBox.startCWD != "/workspace" || launchBox.startEnv["TOKEN"] != "value" {
				t.Fatal("wrong Codex launch", launchBox.startCommand, launchBox.startCWD, launchBox.startEnv)
			}
			if _, ok := launchBox.startEnv["CODEX_HOME"]; ok {
				t.Fatal("Codex home overridden", launchBox.startEnv)
			}
			if err := d.Initialize(t.Context(), source, true); err != nil {
				t.Fatal(err)
			}
			if err := d.Initialize(t.Context(), source, false); err != nil {
				t.Fatal(err)
			}
			if mode == "api_key" {
				if logins != 1 || reads != 0 {
					t.Fatal(logins, reads)
				}
				t.Setenv("FIXTURE_KEY", "")
				if err := d.Initialize(t.Context(), source, true); err == nil {
					t.Fatal("missing key accepted")
				}
			} else {
				if logins != 0 || reads != 2 {
					t.Fatal(logins, reads)
				}
				accountType = "apiKey"
				if err := d.Initialize(t.Context(), source, false); err == nil {
					t.Fatal("wrong account accepted")
				}
			}
		})
	}
}

func TestRecoverResolvesCodexHomeOnlyForMissingHistoryPath(t *testing.T) {
	thread, source, _ := historyFixture(t)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(home, "sessions")
	if err := os.MkdirAll(sessions, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, thread.ID+".jsonl"), data, 0600); err != nil {
		t.Fatal(err)
	}
	box := &testBox{}
	resolved := 0
	box.run = func(ctx context.Context, command string) ([]byte, error) {
		if strings.Contains(command, "${CODEX_HOME:-$HOME/.codex}") {
			resolved++
		}
		return exec.CommandContext(ctx, "sh", "-c", command).Output()
	}
	d := New(box, time.Second, 524288)
	if got, err := d.Recover(t.Context(), &thread.ID, nil, nil); err != nil || len(got.Turns) == 0 || resolved != 1 {
		t.Fatal("missing-path recovery", got, resolved, err)
	}
	if got, err := d.Recover(t.Context(), &thread.ID, &source, nil); err != nil || len(got.Turns) == 0 || resolved != 1 {
		t.Fatal("known-path recovery resolved home", got, resolved, err)
	}
}

func TestContextCreateResumeAndLoss(t *testing.T) {
	var calls []string
	reject := false
	rpc, box := newRPC(t, func(req map[string]json.RawMessage) any {
		var method string
		_ = json.Unmarshal(req["method"], &method)
		calls = append(calls, method)
		if reject {
			return map[string]any{"id": req["id"], "error": map[string]any{"code": 1, "message": "private missing thread"}}
		}
		var params map[string]any
		_ = json.Unmarshal(req["params"], &params)
		if params["model"] != "model" || params["cwd"] != "/workspace" || params["developerInstructions"] != "instructions" || params["approvalPolicy"] != "never" {
			t.Fatal(params)
		}
		if _, ok := params["baseInstructions"]; ok {
			t.Fatal("workflow replaced Codex base instructions", params)
		}
		return map[string]any{"id": req["id"], "result": map[string]any{"thread": map[string]string{"id": "thread", "path": "/history"}}}
	})
	d := New(box, time.Second, 1024)
	d.rpc = rpc
	agent := session.AgentConfiguration{Model: "model", Instructions: "instructions"}
	created, err := d.OpenContext(t.Context(), agent, "/workspace", nil)
	if err != nil || created.NativeID != "thread" || *created.HistoryPath != "/history" {
		t.Fatal(created, err)
	}
	if _, err := d.OpenContext(t.Context(), agent, "/workspace", &created.NativeID); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"thread/start", "thread/resume"}) {
		t.Fatal(calls)
	}
	reject = true
	_, err = d.OpenContext(t.Context(), agent, "/workspace", &created.NativeID)
	f, ok := errors.AsType[*harness.ExecutionError](err)
	if !ok || f.Code != "context_lost" || strings.Contains(err.Error(), "private") {
		t.Fatal(err)
	}
}

func TestContextWithoutInstructionsAndTurnEffort(t *testing.T) {
	var calls []map[string]any
	rpc, box := newRPC(t, func(req map[string]json.RawMessage) any {
		var method string
		_ = json.Unmarshal(req["method"], &method)
		var params map[string]any
		_ = json.Unmarshal(req["params"], &params)
		calls = append(calls, map[string]any{"method": method, "params": params})
		result := map[string]any{}
		switch method {
		case "thread/start":
			result["thread"] = map[string]string{"id": "thread"}
		case "turn/start":
			result["turn"] = map[string]string{"id": "turn"}
		}
		return map[string]any{"id": req["id"], "result": result}
	})
	d := New(box, time.Second, 1024)
	d.rpc = rpc
	agent := session.AgentConfiguration{Model: "model", Codex: session.CodexConfiguration{Effort: "high"}}
	if _, err := d.OpenContext(t.Context(), agent, "/workspace", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Start(t.Context(), agent, "thread", "first"); err != nil {
		t.Fatal(err)
	}
	agent.Codex.Effort = ""
	if _, err := d.Start(t.Context(), agent, "thread", "second"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 {
		t.Fatal(calls)
	}
	thread := calls[0]["params"].(map[string]any)
	if _, ok := thread["developerInstructions"]; ok {
		t.Fatal("empty developer instructions were sent", thread)
	}
	if _, ok := thread["baseInstructions"]; ok {
		t.Fatal("base instructions were replaced", thread)
	}
	first := calls[1]["params"].(map[string]any)
	second := calls[2]["params"].(map[string]any)
	if first["effort"] != "high" || first["threadId"] != "thread" {
		t.Fatal(first)
	}
	if _, ok := second["effort"]; ok {
		t.Fatal("unset effort was sent", second)
	}
}

func TestRecoveryRequiresOneMatchingHistory(t *testing.T) {
	for _, count := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			_, source, _ := historyFixture(t)
			raw, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			home := t.TempDir()
			id := "context[1]"
			for i := range count {
				dir := filepath.Join(home, "sessions", fmt.Sprint(i))
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := recoverHistory(t.Context(), &testBox{}, &id, nil, &home, 1024)
			if count == 1 {
				if err != nil || len(got.Turns) == 0 {
					t.Fatal(got, err)
				}
			} else {
				f, ok := errors.AsType[*harness.ExecutionError](err)
				if !ok || f.Code != "context_lost" {
					t.Fatal(err)
				}
			}
		})
	}
}
