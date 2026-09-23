// Command orpheus-hook-runner records a sandbox hook's result across worker restarts.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

type started struct {
	OperationID string    `json:"operation_id"`
	WrapperPID  int       `json:"wrapper_pid"`
	HookPID     *int      `json:"hook_pid"`
	StartedAt   time.Time `json:"started_at"`
}

type result struct {
	OperationID        string    `json:"operation_id"`
	FinishedAt         time.Time `json:"finished_at"`
	ExitCode           *int      `json:"exit_code"`
	Signal             *int      `json:"signal"`
	OutputCompleteness string    `json:"output_completeness"`
	OriginalBytes      int64     `json:"original_bytes"`
	HeadFile           string    `json:"head_file"`
	TailFile           string    `json:"tail_file,omitzero"`
}

type boundedOutput struct {
	mu    sync.Mutex
	limit int
	total int64
	head  []byte
	tail  []byte
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	b.total += int64(n)
	headLimit := (b.limit + 1) / 2
	if len(b.head) < headLimit {
		take := min(len(p), headLimit-len(b.head))
		b.head = append(b.head, p[:take]...)
		p = p[take:]
	}
	tailLimit := b.limit - headLimit
	switch {
	case len(p) >= tailLimit:
		b.tail = append(b.tail[:0], p[len(p)-tailLimit:]...)
	case len(b.tail)+len(p) > tailLimit:
		drop := len(b.tail) + len(p) - tailLimit
		copy(b.tail, b.tail[drop:])
		b.tail = b.tail[:len(b.tail)-drop]
		b.tail = append(b.tail, p...)
	default:
		b.tail = append(b.tail, p...)
	}
	return n, nil
}

func cleanUTF8(b []byte) string {
	return strings.ToValidUTF8(string(b), "�")
}

func trimSplitRune(b []byte, atStart bool) []byte {
	if atStart {
		for len(b) > 0 && b[0]&0xc0 == 0x80 {
			b = b[1:]
		}
		return b
	}
	start := len(b) - 1
	for start >= 0 && b[start]&0xc0 == 0x80 {
		start--
	}
	if start >= 0 && !utf8.FullRune(b[start:]) {
		return b[:start]
	}
	return b
}

func prefixUTF8(s string, limit int) string {
	end := 0
	for index, r := range s {
		size := utf8.RuneLen(r)
		if index+size > limit {
			break
		}
		end = index + size
	}
	return s[:end]
}

func suffixUTF8(s string, limit int) string {
	start := len(s)
	for start > 0 {
		_, size := utf8.DecodeLastRuneInString(s[:start])
		if len(s)-start+size > limit {
			break
		}
		start -= size
	}
	return s[start:]
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".result-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeAtomic(path, b, 0600)
}

func run(operationID, dir, script, workspace string, limit int) error {
	if operationID == "" || dir == "" || script == "" || workspace == "" || limit < 2 {
		return errors.New("invalid runner arguments")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	claim, err := os.OpenFile(filepath.Join(dir, "claim"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("hook operation already claimed or unavailable: %w", err)
	}
	if err := claim.Close(); err != nil {
		return err
	}
	start := started{OperationID: operationID, WrapperPID: os.Getpid(), StartedAt: time.Now().UTC()}
	if err := writeJSON(filepath.Join(dir, "started.json"), start); err != nil {
		return err
	}

	output := &boundedOutput{limit: limit}
	cmd := exec.Command(script)
	cmd.Dir = workspace
	cmd.Stdout, cmd.Stderr = output, output
	// A background child may inherit stdout after the script has exited.
	// Waiting for that pipe forever would keep the run open indefinitely.
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return writeResult(dir, operationID, nil, err, output)
	}
	start.HookPID = new(cmd.Process.Pid)
	if err := writeJSON(filepath.Join(dir, "started.json"), start); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	waitErr := cmd.Wait()
	return writeResult(dir, operationID, cmd.ProcessState, waitErr, output)
}

func writeResult(dir, operationID string, state *os.ProcessState, waitErr error, output *boundedOutput) error {
	var exitCode, signal *int
	if state != nil {
		if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			signal = new(int(status.Signal()))
		} else {
			exitCode = new(state.ExitCode())
		}
	}
	if waitErr != nil && state != nil && exitCode == nil && signal == nil {
		return waitErr
	}
	rawFull := output.total <= int64(output.limit)
	headBytes, tailBytes := output.head, output.tail
	if !rawFull {
		headBytes = trimSplitRune(headBytes, false)
		tailBytes = trimSplitRune(tailBytes, true)
	}
	full := rawFull
	head, tail := cleanUTF8(headBytes), cleanUTF8(tailBytes)
	if full {
		head += tail
		tail = ""
		full = len(head) <= output.limit
	}
	if !full {
		// The UTF-8 replacement rune can expand invalid input bytes. Keep the
		// persisted result within the configured byte limit even then.
		head = prefixUTF8(head, (output.limit+1)/2)
		if tail == "" {
			tail = suffixUTF8(cleanUTF8(append(append([]byte(nil), output.head...), output.tail...)), output.limit/2)
		} else {
			tail = suffixUTF8(tail, output.limit/2)
		}
	}
	if err := writeAtomic(filepath.Join(dir, "head.txt"), []byte(head), 0600); err != nil {
		return err
	}
	r := result{OperationID: operationID, FinishedAt: time.Now().UTC(), ExitCode: exitCode, Signal: signal, OutputCompleteness: "complete", OriginalBytes: output.total, HeadFile: "head.txt"}
	if !full {
		if err := writeAtomic(filepath.Join(dir, "tail.txt"), []byte(tail), 0600); err != nil {
			return err
		}
		r.TailFile = "tail.txt"
		r.OutputCompleteness = "truncated"
	}
	return writeJSON(filepath.Join(dir, "result.json"), r)
}

func main() {
	var operationID, dir, script, workspace string
	var limit int
	flag.StringVar(&operationID, "operation-id", "", "hook operation UUID")
	flag.StringVar(&dir, "operation-dir", "", "private operation directory")
	flag.StringVar(&script, "script", "", "executable hook script")
	flag.StringVar(&workspace, "workspace", "", "working directory")
	flag.IntVar(&limit, "max-output-bytes", 524288, "maximum retained output bytes")
	flag.Parse()
	if err := run(operationID, dir, script, workspace, limit); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
