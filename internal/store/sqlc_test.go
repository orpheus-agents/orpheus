//go:build integration

package store

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/session"
)

func TestMutateDetectsInPlaceChangesAndSkipsUnchangedSession(t *testing.T) {
	s := fixture(t)
	a, err := s.Accept(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(fn func(*SessionRecord)) {
		t.Helper()
		if err := s.Mutate(t.Context(), a.SessionID, false, func(_ pgx.Tx, record *SessionRecord) error { fn(record); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	mutate(func(r *SessionRecord) {
		r.HistoryPath = new("old")
		r.SandboxError = &session.Error{Code: "fixture", Phase: new("old"), Details: []session.Detail{{Path: []any{"old"}, Code: "fixture"}}}
	})
	mutate(func(r *SessionRecord) { *r.HistoryPath = "new" })
	mutate(func(r *SessionRecord) { *r.SandboxError.Phase = "new" })
	mutate(func(r *SessionRecord) { r.SandboxError.Details[0].Path[0] = "new" })
	r, _, err := s.Read(t.Context(), a.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if *r.HistoryPath != "new" || *r.SandboxError.Phase != "new" || r.SandboxError.Details[0].Path[0] != "new" {
		t.Fatal("in-place changes lost", r)
	}
	_, err = s.Pool.Exec(t.Context(), `CREATE FUNCTION reject_session_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'unexpected session write'; END $$;
CREATE TRIGGER reject_session_write BEFORE UPDATE ON sessions FOR EACH ROW EXECUTE FUNCTION reject_session_write()`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.Pool.Exec(context.Background(), `DROP TRIGGER reject_session_write ON sessions; DROP FUNCTION reject_session_write()`)
	})
	mutate(func(_ *SessionRecord) {})
}

func TestTypedStoragePreservesNullableFieldsAndLargeSequences(t *testing.T) {
	s := fixture(t)
	a, err := s.Accept(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	const sequence int64 = 9007199254740993
	stamp := time.Date(2026, 9, 22, 12, 13, 14, 123456000, time.UTC)
	launchID := uuid.New()
	problem := &session.Error{Code: "fixture", Message: "detail", Phase: new("execution"), Details: []session.Detail{}}
	messageID := uuid.New()
	if err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, record *SessionRecord) error {
		record.ProcessID, record.LaunchID = new(0), &launchID
		record.SandboxError = problem
		record.NextEventSequence = sequence
		run, err := GetRun(t.Context(), tx, a.SessionID, a.RunID)
		if err != nil {
			return err
		}
		run.ExecutionStartedAt, run.DeadlineAt, run.Error = &stamp, new(stamp.Add(time.Hour)), problem
		if err := SaveRun(t.Context(), tx, &run); err != nil {
			return err
		}
		message := MessageRecord{Message: session.Message{
			ID: messageID, SessionID: a.SessionID, RunID: a.RunID,
			Role: "assistant", Kind: new("answer"), Text: "answer", CreatedAt: stamp,
			Position: &session.Position{RunNumber: 1, ItemIndex: 0},
		}}
		return PublishMessage(t.Context(), tx, record, &message)
	}); err != nil {
		t.Fatal(err)
	}
	record, run, err := s.Read(t.Context(), a.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if record.ProcessID == nil || *record.ProcessID != 0 || record.LaunchID == nil || *record.LaunchID != launchID || !reflect.DeepEqual(record.SandboxError, problem) {
		t.Fatalf("session nullable fields changed: %+v", record)
	}
	if run == nil || run.ExecutionStartedAt == nil || !run.ExecutionStartedAt.Equal(stamp) || run.DeadlineAt == nil || !run.DeadlineAt.Equal(stamp.Add(time.Hour)) || !reflect.DeepEqual(run.Error, problem) {
		t.Fatalf("run fields changed: %+v", run)
	}
	message, err := GetMessage(t.Context(), s.Pool, messageID)
	if err != nil {
		t.Fatal(err)
	}
	if message.RegisteredSequence != strconv.FormatInt(sequence, 10) || message.DeliveryNumber != nil || message.DeliveryStatus != nil || message.Error != nil || !message.CreatedAt.Equal(stamp) || message.Position == nil || message.Position.ItemIndex != 0 {
		t.Fatalf("message fields changed: %+v", message)
	}
	events, err := s.Events(t.Context(), a.SessionID, strconv.FormatInt(sequence-1, 10), 10)
	if err != nil || len(events.Items) != 1 || events.Items[0].ID != message.RegisteredSequence {
		t.Fatalf("event sequence changed: %+v, %v", events, err)
	}
	history, err := s.History(t.Context(), a.SessionID, &a.RunID, 10, "", nil)
	if err != nil || len(history.Items) != 2 || history.EventCursor != message.RegisteredSequence {
		t.Fatalf("history sequence changed: %+v, %v", history, err)
	}
	// Clearing nullable fields must write SQL NULL and read back nil, not zero
	// UUIDs/timestamps or empty JSON objects.
	if err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, record *SessionRecord) error {
		record.ProcessID, record.LaunchID, record.SandboxError = nil, nil, nil
		run.ExecutionStartedAt, run.DeadlineAt, run.Error = nil, nil, nil
		return SaveRun(t.Context(), tx, run)
	}); err != nil {
		t.Fatal(err)
	}
	record, run, err = s.Read(t.Context(), a.SessionID)
	if err != nil || record.ProcessID != nil || record.LaunchID != nil || record.SandboxError != nil || run == nil || run.ExecutionStartedAt != nil || run.DeadlineAt != nil || run.Error != nil {
		t.Fatalf("cleared fields did not remain nil: %+v %+v %v", record, run, err)
	}
}

func TestTypedOperationsPreserveNullIdentityAndRawJSON(t *testing.T) {
	s := fixture(t)
	a, err := s.Accept(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Operation(t.Context(), a.SessionID, "pause", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	repeat, err := s.Operation(t.Context(), a.SessionID, "pause", nil, nil, nil)
	if err != nil || repeat.ID != first.ID || repeat.RunID != nil || repeat.MessageID != nil || repeat.AttemptedAt != nil || !jsonEqual(repeat.Parameters, json.RawMessage(`{}`)) {
		t.Fatalf("nullable operation identity changed: %+v, %v", repeat, err)
	}
	raw := json.RawMessage(`{"number":9007199254740993}`)
	operation, err := s.Operation(t.Context(), a.SessionID, "start", &a.RunID, &a.MessageID, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetOperation(t.Context(), a.SessionID, operation.ID, "sending", raw); err != nil {
		t.Fatal(err)
	}
	stored, err := s.GetOperation(t.Context(), operation.ID)
	if err != nil || stored == nil || stored.RunID == nil || *stored.RunID != a.RunID || stored.MessageID == nil || *stored.MessageID != a.MessageID || stored.AttemptedAt == nil || !jsonEqual(stored.Parameters, raw) || !jsonEqual(stored.Result, raw) {
		t.Fatalf("operation fields changed: %+v, %v", stored, err)
	}
}
