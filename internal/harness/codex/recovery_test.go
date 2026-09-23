package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/session"
)

func TestHistoryFallbackDiscoversMissingPath(t *testing.T) {
	thread, path, size := historyFixture(t)
	reads := 0
	rpc, box := newRPC(t, func(req map[string]json.RawMessage) any {
		var params struct {
			IncludeTurns bool `json:"includeTurns"`
		}
		if err := json.Unmarshal(req["params"], &params); err != nil {
			t.Fatal(err)
		}
		if params.IncludeTurns {
			t.Fatal("fallback requested unbounded history")
		}
		reads++
		return map[string]any{"id": req["id"], "result": map[string]any{"thread": Thread{ID: thread.ID, Path: &path}}}
	})
	d := New(box, time.Second, 524288)
	d.rpc = rpc
	d.historyOnly = true
	for range 2 {
		snapshot, err := d.Snapshot(t.Context(), thread.ID, nil, 0)
		if err != nil || snapshot.Path == nil || *snapshot.Path != path || snapshot.Offset != size {
			t.Fatal(snapshot, err)
		}
	}
	if reads != 1 {
		t.Fatal("metadata not cached", reads)
	}
}

func TestStartedToolDoesNotHideJournalCompletion(t *testing.T) {
	for _, source := range []string{"cache", "notification"} {
		t.Run(source, func(t *testing.T) {
			thread, path, size := historyFixture(t)
			expected, err := recoverHistory(t.Context(), &testBox{}, nil, &path, nil, 524288)
			if err != nil {
				t.Fatal(err)
			}
			index := slices.IndexFunc(thread.Turns[0].Items, func(item NativeItem) bool { return item.Type == "commandExecution" })
			stub := thread.Turns[0].Items[index]
			stub.Status, stub.ExitCode, stub.AggregatedOutput = "inProgress", nil, nil
			thread.Turns[0].Items = slices.Delete(thread.Turns[0].Items, index, index+1)
			d, _, reads := fixtureDriver(t, &thread)
			if source == "cache" {
				d.thread = &Thread{ID: thread.ID, Turns: []NativeTurn{{ID: thread.Turns[0].ID, Status: "inProgress", Items: []NativeItem{stub}}}}
			} else {
				raw, err := json.Marshal(map[string]any{"method": "item/started", "params": map[string]any{"turnId": thread.Turns[0].ID, "item": stub}})
				if err != nil {
					t.Fatal(err)
				}
				d.rpc.receive(t.Context(), raw)
			}
			for attempt := range 3 {
				snapshot, err := d.Snapshot(t.Context(), thread.ID, &path, 0)
				if err != nil {
					t.Fatal(err)
				}
				if attempt < 2 {
					if snapshot.Offset != 0 || snapshot.Turns[0].Status.Terminal() {
						t.Fatal("committed incomplete tool")
					}
				} else if snapshot.Offset != size || !reflect.DeepEqual(snapshot.Turns, expected.Turns) {
					t.Fatalf("completed tool not recovered: %#v", snapshot)
				}
				d.Committed()
			}
			d.readAt = time.Time{}
			later, err := d.Snapshot(t.Context(), thread.ID, &path, size)
			if err != nil || later.Offset != size || !slices.Equal(identities(later), identities(expected)) || *reads != 4 {
				t.Fatalf("recovered state lost: %#v, %v", later, err)
			}
		})
	}
}

func TestHistoryFallbackDetectsDisconnectedProcess(t *testing.T) {
	thread, path, _ := historyFixture(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var live []byte
	for line := range bytes.SplitSeq(raw, []byte{'\n'}) {
		if len(line) > 0 && !bytes.Contains(line, []byte(`"type":"task_complete"`)) {
			live = append(live, line...)
			live = append(live, '\n')
		}
	}
	if err := os.WriteFile(path, live, 0600); err != nil {
		t.Fatal(err)
	}
	d, box, _ := fixtureDriver(t, &thread)
	d.historyOnly = true
	snapshot, err := d.Snapshot(t.Context(), thread.ID, &path, 0)
	if err != nil || snapshot.Turns[0].Status != session.Running {
		t.Fatal(snapshot, err)
	}
	close(box.stream.out)
	close(box.stream.errout)
	<-d.rpc.done
	if !d.HasUpdates() {
		t.Fatal("disconnect did not request observation")
	}
	if _, err := d.Snapshot(t.Context(), thread.ID, &path, snapshot.Offset); !errors.Is(err, harness.ErrUncertain) {
		t.Fatalf("closed stream did not request recovery: %v", err)
	}
}

func TestStartedMessageDoesNotHideMissingFinalText(t *testing.T) {
	thread, path, size := historyFixture(t)
	expected, err := recoverHistory(t.Context(), &testBox{}, nil, &path, nil, 524288)
	if err != nil {
		t.Fatal(err)
	}
	var placeholders []NativeItem
	for _, item := range thread.Turns[0].Items {
		if item.Type == "agentMessage" {
			item.Text = ""
			placeholders = append(placeholders, item)
		}
	}
	thread.Turns[0].Items = slices.DeleteFunc(thread.Turns[0].Items, func(item NativeItem) bool { return item.Type == "agentMessage" })
	d, _, _ := fixtureDriver(t, &thread)
	d.thread = &Thread{ID: thread.ID, Turns: []NativeTurn{{ID: thread.Turns[0].ID, Status: "inProgress", Items: placeholders}}}
	for attempt := range 3 {
		snapshot, err := d.Snapshot(t.Context(), thread.ID, &path, 0)
		if err != nil {
			t.Fatal(err)
		}
		if attempt < 2 {
			if snapshot.Offset != 0 || snapshot.Turns[0].Status.Terminal() {
				t.Fatal("committed incomplete final answer")
			}
		} else if snapshot.Offset != size || !reflect.DeepEqual(snapshot.Turns, expected.Turns) {
			t.Fatalf("final text not recovered: %#v", snapshot)
		}
	}
}

func TestOldNotificationsCannotRegressCompletedTurn(t *testing.T) {
	for _, count := range []int{1, 1025} {
		thread := Thread{ID: "thread", Turns: []NativeTurn{{ID: "turn", Status: "completed"}}}
		d, _, _ := fixtureDriver(t, &thread)
		for range count {
			d.rpc.receive(t.Context(), []byte(`{"method":"turn/started","params":{"turn":{"id":"turn","status":"inProgress"}}}`))
		}
		snapshot, err := d.Snapshot(t.Context(), thread.ID, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Turns[0].Status != session.Completed {
			t.Fatalf("%d notifications: completed became %s", count, snapshot.Turns[0].Status)
		}
	}
}

func TestOversizedHistoryFallsBackToJournal(t *testing.T) {
	thread, path, size := historyFixture(t)
	expected, err := recoverHistory(t.Context(), &testBox{}, nil, &path, nil, 524288)
	if err != nil {
		t.Fatal(err)
	}
	for range 40 {
		thread.Turns[0].Items = append(thread.Turns[0].Items, NativeItem{Type: "agentMessage", Text: strings.Repeat("x", 450000)})
	}
	d, _, reads := fixtureDriver(t, &thread)
	d.rpc.timeout = 5 * time.Second
	for range 2 {
		snapshot, err := d.Snapshot(t.Context(), thread.ID, &path, 0)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Offset != size || !reflect.DeepEqual(snapshot.Turns, expected.Turns) {
			t.Fatal("oversized RPC history did not recover from JSONL")
		}
		d.Committed()
	}
	if *reads != 1 {
		t.Fatalf("repeated oversized thread/read: %d", *reads)
	}
}
