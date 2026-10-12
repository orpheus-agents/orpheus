package codex

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus/internal/harness"
)

func TestMissingItemTimingPreservesLiveRestartAndRecoveryOrder(t *testing.T) {
	path, err := filepath.Abs("testdata/missing-item-timing.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var history offlineHistory
	if err := readNative(t.Context(), &testBox{}, "offline", path, 0, 1024, &history); err != nil {
		t.Fatal(err)
	}
	history.Path = &path
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	assertOrder := func(snapshot harness.Snapshot) {
		t.Helper()
		var ids []string
		for _, item := range snapshot.Turns[0].Items {
			ids = append(ids, item.NativeID)
		}
		if want := []string{"user", "search", "open", "command", "answer"}; !reflect.DeepEqual(ids, want) {
			t.Fatalf("history order = %v, want %v", ids, want)
		}
	}
	for _, offset := range []int64{0, info.Size()} {
		driver, _, _ := fixtureDriver(t, &history.Thread)
		snapshot, err := driver.Snapshot(t.Context(), "thread", &path, offset)
		if err != nil {
			t.Fatal(err)
		}
		assertOrder(snapshot)
	}
	recovered, err := recoverHistory(t.Context(), &testBox{}, nil, &path, nil, 1024)
	if err != nil {
		t.Fatal(err)
	}
	assertOrder(recovered)
}

func TestLateCompletionKeepsFirstJournalTimeAcrossBatches(t *testing.T) {
	raw, err := os.ReadFile("testdata/missing-item-timing.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	// A search start can be committed before its completion, without explicit timing.
	lines := strings.SplitAfter(string(raw), "\n")
	lines[2] = strings.Replace(lines[2], "item_completed", "item_started", 1)
	path := filepath.Join(t.TempDir(), "history.jsonl")
	first := strings.Join(lines[:3], "")
	if err := os.WriteFile(path, []byte(first), 0600); err != nil {
		t.Fatal(err)
	}
	var history offlineHistory
	if err := readNative(t.Context(), &testBox{}, "offline", path, 0, 1024, &history); err != nil {
		t.Fatal(err)
	}
	history.Path = &path
	driver, _, _ := fixtureDriver(t, &history.Thread)
	snapshot, err := driver.Snapshot(t.Context(), "thread", &path, 0)
	if err != nil {
		t.Fatal(err)
	}
	completion := `{"timestamp":"2026-10-12T13:30:00.000Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"turn","item":{"id":"search","type":"WebSearch"}}}` + "\n"
	if err := os.WriteFile(path, []byte(strings.Join(lines, "")+completion), 0600); err != nil {
		t.Fatal(err)
	}
	if err := readNative(t.Context(), &testBox{}, "offline", path, 0, 1024, &history); err != nil {
		t.Fatal(err)
	}
	history.Path = &path
	driver.readAt = time.Time{}
	snapshot, err = driver.Snapshot(t.Context(), "thread", &path, snapshot.Offset)
	if err != nil {
		t.Fatal(err)
	}
	if driver.orders["search"] != 1791811669635 || snapshot.Turns[0].Items[1].NativeID != "search" {
		t.Fatalf("late completion moved search: %+v, order=%d", snapshot.Turns[0].Items, driver.orders["search"])
	}
}
