package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/session"
)

func historyFixture(t *testing.T) (Thread, string, int64) {
	t.Helper()
	raw, err := os.ReadFile("../../../tests/fixtures/codex-native-history.json")
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
	var data bytes.Buffer
	for _, record := range pair.Records {
		if err := json.Compact(&data, record); err != nil {
			t.Fatal(err)
		}
		data.WriteByte('\n')
	}
	path := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	pair.Thread.Path = &path
	return pair.Thread, path, int64(data.Len())
}

// Captured from Codex 0.154 after thread/inject_items on a fresh thread.
func TestInjectedRolloutItemsHaveNoNativeTurn(t *testing.T) {
	path, err := filepath.Abs("testdata/injected-rollout.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for line := range bytes.SplitSeq(bytes.TrimSpace(raw), []byte{'\n'}) {
		var record struct {
			Type    string `json:"type"`
			Payload struct {
				Role    string `json:"role"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record.Type != "response_item" || record.Payload.Role != "user" || len(record.Payload.Content) != 1 {
			t.Fatalf("unexpected injected record: %s", line)
		}
		texts = append(texts, record.Payload.Content[0].Text)
	}
	if !slices.Equal(texts, []string{"FIRST injected", "SECOND injected"}) {
		t.Fatalf("injected texts: %q", texts)
	}
	snapshot, err := recoverHistory(t.Context(), &testBox{}, nil, &path, nil, 524288)
	if err != nil || len(snapshot.Turns) != 0 {
		t.Fatalf("injected items incorrectly became native turn items: %+v %v", snapshot, err)
	}
}
func fixtureDriver(t *testing.T, thread *Thread) (*Driver, *testBox, *int) {
	t.Helper()
	reads := 0
	rpc, box := newRPC(t, func(req map[string]json.RawMessage) any {
		reads++
		return map[string]any{"id": req["id"], "result": map[string]any{"thread": thread}}
	})
	d := New(box, time.Second, 524288)
	d.rpc = rpc
	return d, box, &reads
}
func identities(s harness.Snapshot) []string {
	out := []string{}
	for _, turn := range s.Turns {
		out = append(out, turn.NativeID+":"+string(turn.Status))
		for _, item := range turn.Items {
			raw, _ := json.Marshal([]any{item.NativeID, item.Index, item.Status, item.Input})
			out = append(out, string(raw))
		}
	}
	return out
}
func TestPersistentNativeOmissionsRecover(t *testing.T) {
	for _, omission := range []string{"turn", "commandExecution", "agentMessage"} {
		t.Run(omission, func(t *testing.T) {
			thread, path, size := historyFixture(t)
			expected, err := recoverHistory(t.Context(), &testBox{}, nil, &path, nil, 524288)
			if err != nil {
				t.Fatal(err)
			}
			if omission == "turn" {
				thread.Turns = nil
			} else {
				thread.Turns[0].Items = slices.DeleteFunc(thread.Turns[0].Items, func(item NativeItem) bool { return item.Type == omission })
			}
			d, box, _ := fixtureDriver(t, &thread)
			offline := 0
			box.run = func(ctx context.Context, command string) ([]byte, error) {
				if strings.Contains(command, "\noffline()\n") {
					offline++
				}
				return exec.CommandContext(ctx, "sh", "-c", command).Output()
			}
			for range 2 {
				snapshot, err := d.Snapshot(t.Context(), thread.ID, &path, 0)
				if err != nil {
					t.Fatal(err)
				}
				if snapshot.Offset != 0 {
					t.Fatal("advanced over omitted item")
				}
				for _, turn := range snapshot.Turns {
					if turn.Status.Terminal() {
						t.Fatal("premature terminal result")
					}
				}
				d.Committed()
			}
			recovered, err := d.Snapshot(t.Context(), thread.ID, &path, 0)
			if err != nil {
				t.Fatal(err)
			}
			if recovered.Offset != size || !reflect.DeepEqual(recovered.Turns, expected.Turns) {
				t.Fatalf("recovery differs: %#v", recovered)
			}
			d.Committed()
			d.readAt = time.Time{}
			later, err := d.Snapshot(t.Context(), thread.ID, &path, size)
			if err != nil {
				t.Fatal(err)
			}
			if offline != 1 || later.Offset != size || !slices.Equal(identities(later), identities(expected)) {
				t.Fatalf("repeated recovery or lost identities: %d %#v", offline, later)
			}
		})
	}
}
func TestSnapshotReplayAndReconnectIndex(t *testing.T) {
	thread, path, size := historyFixture(t)
	d, box, _ := fixtureDriver(t, &thread)
	first, err := d.Snapshot(t.Context(), thread.ID, &path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.Offset != size || len(d.outputs) == 0 {
		t.Fatal("journal not captured")
	}
	// Failed database commit: retain outputs and replay from the persisted offset.
	d.readAt = time.Time{}
	retry, err := d.Snapshot(t.Context(), thread.ID, &path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, retry) {
		t.Fatal("failed commit changed replay")
	}
	d.Committed()
	if len(d.outputs) != 0 {
		t.Fatal("committed results retained")
	}
	restarted, _, _ := fixtureDriver(t, &thread)
	modes := []string{}
	box.run = func(ctx context.Context, command string) ([]byte, error) {
		for _, mode := range []string{"journal", "index", "offline"} {
			if strings.Contains(command, "\n"+mode+"()\n") {
				modes = append(modes, mode)
			}
		}
		return exec.CommandContext(ctx, "sh", "-c", command).Output()
	}
	restarted.rpc.sandbox = box
	restored, err := restarted.Snapshot(t.Context(), thread.ID, &path, size)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(identities(first), identities(restored)) || len(restarted.outputs) != 0 || !slices.Equal(modes, []string{"index", "journal"}) {
		t.Fatalf("restart replayed output or changed order: %v", modes)
	}
	restarted.Committed()
	restarted.readAt = time.Time{}
	if _, err := restarted.Snapshot(t.Context(), thread.ID, &path, size); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(modes, []string{"index", "journal", "journal"}) {
		t.Fatal(modes)
	}
}
func TestNotificationsAvoidPollingAndKeepInterruptedStart(t *testing.T) {
	thread := Thread{ID: "thread", Turns: []NativeTurn{{ID: "turn", Status: "inProgress"}}}
	d, _, reads := fixtureDriver(t, &thread)
	if _, err := d.Snapshot(t.Context(), thread.ID, nil, 0); err != nil {
		t.Fatal(err)
	}
	d.rpc.receive(t.Context(), []byte(`{"method":"item/started","params":{"turnId":"turn","item":{"id":"tool","type":"commandExecution","status":"inProgress","command":"sleep 30"}}}`))
	snapshot, err := d.Snapshot(t.Context(), thread.ID, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if *reads != 1 || len(snapshot.Turns[0].Items) != 1 {
		t.Fatal("notification triggered full read or lost item")
	}
	d.rpc.receive(t.Context(), []byte(`{"method":"turn/completed","params":{"turn":{"id":"turn","status":"interrupted"}}}`))
	snapshot, err = d.Snapshot(t.Context(), thread.ID, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if *reads != 2 || snapshot.Turns[0].Status != session.Cancelled || len(snapshot.Turns[0].Items) != 1 || snapshot.Turns[0].Items[0].Status != "unknown" {
		t.Fatalf("interrupted start lost: %#v", snapshot)
	}
}
