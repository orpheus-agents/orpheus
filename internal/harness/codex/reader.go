package codex

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/skillum-ai/orpheus/internal/harness"
)

//go:embed native_reader.py
var readerSource string

type journalItem struct {
	Type        string          `json:"type"`
	ID          string          `json:"id"`
	StartedAtMS int64           `json:"started_at_ms"`
	Argv        json.RawMessage `json:"argv"`
	Output
}
type journalBatch struct {
	Items     []journalItem `json:"items"`
	Offset    int64         `json:"offset"`
	Exhausted bool          `json:"exhausted"`
}
type journalIndex struct {
	Orders    map[string]int64           `json:"orders"`
	Commands  map[string]json.RawMessage `json:"commands"`
	Completed []string                   `json:"completed"`
}
type offlineHistory struct {
	Thread
	Completed []string          `json:"completed"`
	Outputs   map[string]Output `json:"outputs"`
	Offset    int64             `json:"offset"`
}

func readNative(ctx context.Context, s harness.Sandbox, mode, path string, offset int64, limit int, dest any) error {
	command := "python3 -c " + harness.Quote(readerSource+"\n"+mode+"()\n") + " " + harness.Quote(path) + fmt.Sprintf(" %d %d", offset, limit)
	raw, err := s.Run(ctx, command)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, dest)
}
func recoverHistory(ctx context.Context, s harness.Sandbox, contextID, path, home *string, limit int) (harness.Snapshot, error) {
	if path == nil && contextID != nil && home != nil {
		dir := harness.Quote(*home + "/sessions")
		out, err := s.Run(ctx, "if [ -d "+dir+" ]; then find "+dir+" -type f -name '*.jsonl' -print0; fi")
		if err != nil {
			return harness.Snapshot{}, err
		}
		var matches []string
		for candidate := range bytes.SplitSeq(bytes.TrimSuffix(out, []byte{0}), []byte{0}) {
			name := string(candidate)
			if name != "" && strings.Contains(filepath.Base(name), *contextID) {
				matches = append(matches, name)
			}
		}
		if len(matches) == 1 {
			path = &matches[0]
		}
	}
	if path == nil {
		return harness.Snapshot{}, harness.Failure("context_lost", "Harness history is unavailable.")
	}
	var history offlineHistory
	if err := readNative(ctx, s, "offline", *path, 0, limit, &history); err != nil {
		return harness.Snapshot{}, err
	}
	complete := map[string]bool{}
	for _, id := range history.Completed {
		complete[id] = true
	}
	snapshot := Normalize(history.Thread, complete, history.Outputs, limit, nil, nil)
	snapshot.Path = path
	snapshot.Offset = history.Offset
	return snapshot, nil
}
