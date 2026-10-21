package store

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/orpheus-agents/orpheus/internal/harness"
)

func TestSanitizeJSONNUL(t *testing.T) {
	for _, test := range []struct {
		name, raw, want string
	}{
		{"object", `{"nul":"\u0000","literal":"\\u0000","slashes":"\\\\\u0000"}`, `{"nul":"\ufffd","literal":"\\u0000","slashes":"\\\\\ufffd"}`},
		{"adjacent", `"\u0000\u0000"`, `"\ufffd\ufffd"`},
		{"edges", `"\u0000middle\u0000"`, `"\ufffdmiddle\ufffd"`},
		{"key", `{"\u0000":"value"}`, `{"\ufffd":"value"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := sanitizeJSON(json.RawMessage(test.raw))
			if string(got) != test.want {
				t.Fatalf("got %s, want %s", got, test.want)
			}
			if !json.Valid(got) {
				t.Fatal("sanitizer returned invalid JSON", string(got))
			}
		})
	}
	if sanitizeJSON(nil) != nil {
		t.Fatal("nil JSON changed")
	}
}

func TestSanitizeSnapshotNUL(t *testing.T) {
	snapshot := harness.Snapshot{
		Path:  new("history\x00path"),
		Usage: []harness.UsageReport{{ContextID: "context\x00id", TurnID: "turn\x00id"}},
		Turns: []harness.Turn{{
			NativeID:     "turn\x00id",
			ErrorCode:    "error\x00code",
			ErrorMessage: "error\x00message",
			Items: []harness.Item{{
				NativeID:         "item\x00id",
				Type:             "tool\x00type",
				Text:             "text\x00value",
				Kind:             new("kind\x00value"),
				Name:             "tool\x00name",
				Input:            json.RawMessage(`{"value":"\u0000"}`),
				Status:           "status\x00value",
				Result:           json.RawMessage(`{"text":"\u0000"}`),
				Completeness:     "complete\x00value",
				TruncationReason: new("reason\x00value"),
			}},
		}},
	}
	original := snapshot
	sanitizeSnapshot(&snapshot)
	sanitized := snapshot
	sanitized.Usage = nil // Usage is sanitized independently by ApplyUsage.
	raw, err := json.Marshal(sanitized)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, jsonNUL) {
		t.Fatal("snapshot still contains a NUL escape", string(raw))
	}
	if *snapshot.Path != "history\uFFFDpath" || snapshot.Turns[0].Items[0].Text != "text\uFFFDvalue" {
		t.Fatal("snapshot text was not sanitized", snapshot)
	}
	if *original.Path != "history\x00path" || original.Turns[0].Items[0].Text != "text\x00value" {
		t.Fatal("source snapshot was mutated", original)
	}
	reports := []harness.UsageReport{{ContextID: "context\x00id", TurnID: "turn\x00id"}}
	cleanReports := sanitizeUsageReports(reports)
	if reports[0].ContextID != "context\x00id" || cleanReports[0].ContextID != "context\uFFFDid" || cleanReports[0].TurnID != "turn\uFFFDid" {
		t.Fatal("usage reports were not copied and sanitized", reports, cleanReports)
	}
}
