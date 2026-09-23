//go:build integration

package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/session"
)

func TestLargeIntegerProjectionChanges(t *testing.T) {
	s := fixture(t)
	a, err := s.Accept(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	for i, number := range []string{"9007199254740992", "9007199254740993"} {
		err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, r *SessionRecord) error {
			r.ThreadID = new("thread")
			run, err := GetRun(t.Context(), tx, a.SessionID, a.RunID)
			if err != nil {
				return err
			}
			run.NativeTurnID = new("turn")
			if err := SaveRun(t.Context(), tx, &run); err != nil {
				return err
			}
			item := harness.Item{NativeID: "tool", Type: "tool", Index: 1, Name: "native", Input: json.RawMessage(`{"id":` + number + `}`), Status: "completed", Result: json.RawMessage(`{"type":"json","value":` + number + `,"original_bytes":16}`), Completeness: "complete"}
			return Reconcile(t.Context(), tx, r, harness.Snapshot{Turns: []harness.Turn{{NativeID: "turn", Status: session.Running, Items: []harness.Item{item}}}})
		})
		if err != nil {
			t.Fatal(err)
		}
		page, err := s.History(t.Context(), a.SessionID, nil, 50, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, item := range page.Items {
			if item.ToolCall != nil {
				found = true
				if !strings.Contains(string(item.ToolCall.Input), number) || !strings.Contains(string(item.ToolCall.Result), number) {
					t.Fatalf("integer change lost: %+v", item.ToolCall)
				}
			}
		}
		if !found {
			t.Fatal("missing tool")
		}
		var count int
		if err := s.Pool.QueryRow(t.Context(), "SELECT count(*) FROM session_events WHERE session_id=$1 AND type='tool_call.updated'", a.SessionID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != i+1 {
			t.Fatal("changed tool was not published", count)
		}
	}
}

func TestMalformedCursorComponents(t *testing.T) {
	s := fixture(t)
	a, err := s.Accept(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	scope, err := (ListFilter{}).scope("runs:" + a.SessionID.String())
	if err != nil {
		t.Fatal(err)
	}
	for _, position := range []string{"null", "true", "1.0", "-1"} {
		_, err := s.ListRuns(t.Context(), a.SessionID, 50, encodeCursor(scope, json.RawMessage(position)), ListFilter{})
		requireCode(t, err, "invalid_cursor")
	}
	for i, position := range []string{`{"Last":[0,0,0,0]}`, `{"Watermark":null,"Last":[0,0,0,0]}`, `{"Watermark":0,"Last":[null,0,0,0]}`, `{"Watermark":0,"Last":[0,0,0]}`, `{"Watermark":true,"Last":[0,0,0,0]}`} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			_, err := s.History(t.Context(), a.SessionID, nil, 50, encodeCursor(historyScope(a.SessionID, nil, nil), json.RawMessage(position)), nil)
			requireCode(t, err, "invalid_cursor")
		})
	}
}

func TestEquivalentDecimalsDoNotPublishAgain(t *testing.T) {
	s := fixture(t)
	a, err := s.Accept(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	for _, number := range []string{"1e3", "1e3", "1000.00", "10e2"} {
		if err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, r *SessionRecord) error {
			r.ThreadID = new("thread")
			run, err := GetRun(t.Context(), tx, a.SessionID, a.RunID)
			if err != nil {
				return err
			}
			run.NativeTurnID = new("turn")
			if err := SaveRun(t.Context(), tx, &run); err != nil {
				return err
			}
			item := harness.Item{NativeID: "tool", Type: "tool", Name: "native", Input: json.RawMessage(`{"value":` + number + `}`), Status: "completed", Result: json.RawMessage(`{"type":"json","value":` + number + `}`), Completeness: "complete"}
			return Reconcile(t.Context(), tx, r, harness.Snapshot{Turns: []harness.Turn{{NativeID: "turn", Status: session.Running, Items: []harness.Item{item}}}})
		}); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := s.Pool.QueryRow(t.Context(), "SELECT count(*) FROM session_events WHERE session_id=$1 AND type='tool_call.updated'", a.SessionID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("equivalent decimals emitted %d events", count)
	}
}

func TestProjectionEventTimestampMatchesStoredValue(t *testing.T) {
	s := fixture(t)
	a, err := s.Accept(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 9, 22, 12, 0, 0, 123456789, time.UTC)
	if err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, r *SessionRecord) error {
		message := MessageRecord{ID: uuid.New(), SessionID: a.SessionID, RunID: a.RunID, Role: "assistant", Kind: new("answer"), Text: "answer", CreatedAt: created}
		return PublishMessage(t.Context(), tx, r, &message)
	}); err != nil {
		t.Fatal(err)
	}
	var stored time.Time
	var event string
	if err := s.Pool.QueryRow(t.Context(), `SELECT m.created_at,e.data->>'created_at' FROM messages m JOIN session_events e ON e.session_id=m.session_id AND e.data->>'id'=m.id::text WHERE m.session_id=$1 AND m.role='assistant'`, a.SessionID).Scan(&stored, &event); err != nil {
		t.Fatal(err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, event)
	if err != nil || !stored.Equal(parsed) {
		t.Fatal("event and GET timestamps differ", stored, event, err)
	}
}
