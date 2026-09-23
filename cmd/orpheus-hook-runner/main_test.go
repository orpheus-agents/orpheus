package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRunRecordsResultWithoutReexecution(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "operation")
	script := filepath.Join(root, "hook")
	content := "#!/bin/sh\nprintf 'called\\n' >> calls\nprintf 'hello'; printf ' world' >&2\nexit 7\n"
	if err := os.WriteFile(script, []byte(content), 0700); err != nil {
		t.Fatal(err)
	}
	if err := run("operation-1", dir, script, root, 32); err != nil {
		t.Fatal(err)
	}
	var got result
	data, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ExitCode == nil || *got.ExitCode != 7 || got.Signal != nil || got.OutputCompleteness != "complete" || got.OriginalBytes != 11 {
		t.Fatalf("unexpected hook result: %+v", got)
	}
	output, err := os.ReadFile(filepath.Join(dir, got.HeadFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "hello world" {
		t.Fatalf("unexpected output %q", output)
	}
	if err := run("operation-1", dir, script, root, 32); err == nil {
		t.Fatal("claimed hook operation ran again")
	}
	calls, err := os.ReadFile(filepath.Join(root, "calls"))
	if err != nil || string(calls) != "called\n" {
		t.Fatalf("hook execution count: %q, %v", calls, err)
	}
}

func TestRunKeepsOnlyOutputEdges(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "operation")
	script := filepath.Join(root, "hook")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'abcdefghijklmnopqrstuvwxyz'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := run("operation-2", dir, script, root, 10); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got result
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.OutputCompleteness != "truncated" || got.OriginalBytes != 26 {
		t.Fatalf("unexpected output metadata: %+v", got)
	}
	head, err := os.ReadFile(filepath.Join(dir, got.HeadFile))
	if err != nil {
		t.Fatal(err)
	}
	tail, err := os.ReadFile(filepath.Join(dir, got.TailFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(head) != "abcde" || string(tail) != "vwxyz" || strings.Contains(string(head)+string(tail), "fgh") {
		t.Fatalf("wrong retained edges: %q %q", head, tail)
	}
}

func TestInvalidUTF8StillRespectsOutputLimit(t *testing.T) {
	dir := t.TempDir()
	output := &boundedOutput{limit: 8}
	if _, err := output.Write([]byte{0xff, 'a', 0xfe, 'b', 0xfd, 'c', 0xfc, 'd', 0xfb, 'e', 0xfa, 'f', 0xf9, 'g', 0xf8, 'h'}); err != nil {
		t.Fatal(err)
	}
	if err := writeResult(dir, "invalid-utf8", nil, nil, output); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got result
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	head, err := os.ReadFile(filepath.Join(dir, got.HeadFile))
	if err != nil {
		t.Fatal(err)
	}
	tail, err := os.ReadFile(filepath.Join(dir, got.TailFile))
	if err != nil {
		t.Fatal(err)
	}
	if got.OutputCompleteness != "truncated" || len(head)+len(tail) > 8 || !utf8.Valid(head) || !utf8.Valid(tail) {
		t.Fatal(got, head, tail)
	}
}

func TestMissingInterpreterRecordsLaunchFailure(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "operation")
	script := filepath.Join(root, "hook")
	if err := os.WriteFile(script, []byte("#!/no-such-hook-interpreter\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := run("missing-interpreter", dir, script, root, 32); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got result
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ExitCode != nil || got.Signal != nil || got.OutputCompleteness != "complete" {
		t.Fatalf("unexpected launch failure result: %+v", got)
	}
}

func TestLargeOutputContinuesAfterLimit(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "operation")
	script := filepath.Join(root, "hook")
	content := "#!/bin/sh\ni=0; while [ \"$i\" -lt 10000 ]; do printf x; i=$((i+1)); done\nprintf done > marker\n"
	if err := os.WriteFile(script, []byte(content), 0700); err != nil {
		t.Fatal(err)
	}
	if err := run("large-output", dir, script, root, 10); err != nil {
		t.Fatal(err)
	}
	if marker, err := os.ReadFile(filepath.Join(root, "marker")); err != nil || string(marker) != "done" {
		t.Fatalf("hook did not finish after output limit: %q, %v", marker, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got result
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 || got.OriginalBytes != 10000 || got.OutputCompleteness != "truncated" {
		t.Fatalf("unexpected large output result: %+v", got)
	}
}

func TestRunDoesNotWaitForBackgroundChildOutput(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "operation")
	script := filepath.Join(root, "hook")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 3600 & echo $! > child.pid\necho done\n"), 0700); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now()
	if err := run("background-child", dir, script, root, 32); err != nil {
		t.Fatal(err)
	}
	child, err := os.ReadFile(filepath.Join(root, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(child)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if time.Since(startedAt) > 5*time.Second {
		t.Fatal("runner waited for background child output")
	}
	data, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got result
	if err := json.Unmarshal(data, &got); err != nil || got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatal(got, err)
	}
}
