//go:build integration

package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/harness"
	"github.com/orpheus-agents/orpheus/internal/secret"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

func fixture(t *testing.T) *Store {
	t.Helper()
	c, err := secret.New(base64.URLEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return &Store{Pool: testutil.Database(t), Settings: config.DefaultSettings(), Cipher: c, Profiles: config.Profiles{Profiles: map[string]config.Profile{"default": {Harness: "codex", Model: new("fixture"), Auth: config.Auth{Mode: "api_key", APIKeyEnv: "OPENAI_API_KEY"}}}}}
}
func request() Admission {
	return Admission{Key: uuid.New(), Create: &session.CreateSession{Configuration: session.ConfigurationInput{Agent: session.AgentInput{Profile: "default"}, Sandbox: session.SandboxInput{Template: "codex", Env: map[string]string{"TOKEN": "private"}}, Limits: session.Limits{RunTimeoutSeconds: 3600}}, Message: session.TextMessage{Text: "Hello"}}}
}
func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	e, ok := errors.AsType[*session.APIError](err)
	if !ok || e.Problem.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}
func TestConcurrentAdmission(t *testing.T) {
	s := fixture(t)
	s.Settings.MaxConcurrentSessions = 1
	// Identical concurrent requests share durable IDs and produce exactly two events.
	req := request()
	req.Create.Configuration.Sandbox.EnvFrom = []string{}
	var wg sync.WaitGroup
	results := make(chan session.Acceptance, 16)
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() { a, err := s.Accept(t.Context(), req); results <- a; errs <- err })
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first session.Acceptance
	for a := range results {
		if first.SessionID == uuid.Nil {
			first = a
		}
		if first != a {
			t.Fatal("idempotency split")
		}
	}
	events, err := s.Events(t.Context(), first.SessionID, "0", 50)
	if err != nil || len(events.Items) != 2 {
		t.Fatal(events, err)
	}
	_, err = s.Accept(t.Context(), request())
	requireCode(t, err, "capacity_exhausted")
	req.Create.Configuration.Sandbox.Env["TOKEN"] = "changed"
	_, err = s.Accept(t.Context(), req)
	requireCode(t, err, "idempotency_conflict")
	_, err = s.Accept(t.Context(), Admission{SessionID: first.SessionID, Key: uuid.New(), Text: "next"})
	requireCode(t, err, "session_busy")
	cancelled, err := s.Cancel(t.Context(), first.SessionID, first.RunID)
	if err != nil || cancelled.Status != session.Cancelled {
		t.Fatal(cancelled, err)
	}
	again, err := s.Cancel(t.Context(), first.SessionID, first.RunID)
	if err != nil || again != cancelled {
		t.Fatal(again, err)
	}
	if _, err := s.Accept(t.Context(), request()); err != nil {
		t.Fatal("slot not released", err)
	}
}
func TestAtomicCapacity(t *testing.T) {
	s := fixture(t)
	s.Settings.MaxConcurrentSessions = 3
	var wg sync.WaitGroup
	results := make(chan error, 20)
	for range 20 {
		wg.Go(func() { _, err := s.Accept(t.Context(), request()); results <- err })
	}
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else {
			requireCode(t, err, "capacity_exhausted")
		}
	}
	if accepted != 3 {
		t.Fatalf("accepted %d", accepted)
	}
}
func TestSnapshotAndHistory(t *testing.T) {
	s := fixture(t)
	a, err := s.Accept(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := GetSession(t.Context(), tx, a.SessionID, true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := s.Session(ctx, a.SessionID); err != nil {
		t.Fatal("GET blocked on worker row lock", err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Register two answers, then mutate one after page 1. Page 2 must use its watermark.
	ids := []uuid.UUID{uuid.New(), uuid.New()}
	err = s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, r *SessionRecord) error {
		for i, id := range ids {
			m := MessageRecord{Message: session.Message{ID: id, SessionID: a.SessionID, RunID: a.RunID, Role: "assistant", Kind: new("answer"), Text: fmt.Sprint(i), Position: &session.Position{RunNumber: 1, ItemIndex: i + 1}}}
			if err := PublishMessage(t.Context(), tx, r, &m); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.History(t.Context(), a.SessionID, nil, 1, "", nil)
	if err != nil || page.NextCursor == nil {
		t.Fatal(page, err)
	}
	err = s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, r *SessionRecord) error {
		m, err := GetMessage(t.Context(), tx, ids[1])
		if err != nil {
			return err
		}
		m.Text = "changed"
		return PublishMessage(t.Context(), tx, r, &m)
	})
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.History(t.Context(), a.SessionID, nil, 1, *page.NextCursor, nil)
	if err != nil || next.EventCursor != page.EventCursor || next.Items[0].Message.Text != "1" {
		t.Fatal(next, err)
	}
	tail, err := s.Events(t.Context(), a.SessionID, page.EventCursor, 50)
	if err != nil || len(tail.Items) != 1 {
		t.Fatal(tail, err)
	}
	for _, after := range []string{"+1", "-1", "9999999999999999999", "١", ""} {
		_, err := s.Events(t.Context(), a.SessionID, after, 50)
		requireCode(t, err, "invalid_cursor")
	}
	other, err := s.Accept(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.History(t.Context(), other.SessionID, nil, 1, *page.NextCursor, nil)
	requireCode(t, err, "invalid_cursor")
}
func TestSteerAndFinish(t *testing.T) {
	s := fixture(t)
	a, err := s.Accept(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, record *SessionRecord) error {
		r, err := GetRun(t.Context(), tx, a.SessionID, a.RunID)
		if err != nil {
			return err
		}
		r.Status = session.Running
		return PublishRun(t.Context(), tx, record, &r)
	}); err != nil {
		t.Fatal(err)
	}
	b, err := s.Accept(t.Context(), Admission{SessionID: a.SessionID, RunID: a.RunID, Key: uuid.New(), Text: "same"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Accept(t.Context(), Admission{SessionID: a.SessionID, RunID: a.RunID, Key: uuid.New(), Text: "same"})
	if err != nil || b.MessageID == c.MessageID {
		t.Fatal(c, err)
	}
	if err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, record *SessionRecord) error {
		r, err := GetRun(t.Context(), tx, a.SessionID, a.RunID)
		if err != nil {
			return err
		}
		return Finish(t.Context(), tx, record, &r, session.Completed, nil, nil)
	}); err != nil {
		t.Fatal(err)
	}
	m, err := GetMessage(t.Context(), s.Pool, b.MessageID)
	if err != nil || *m.DeliveryStatus != "rejected" {
		t.Fatal(m, err)
	}
	if _, err := s.Accept(t.Context(), Admission{SessionID: a.SessionID, Key: uuid.New(), Text: "next"}); err != nil {
		t.Fatal(err)
	}
}

type auditedTx struct {
	pgx.Tx
	selects, resultReads, directWrites, batches int
}

func (a *auditedTx) query(sql string) {
	a.selects++
	if strings.Contains(sql, "FROM tool_calls t") {
		a.resultReads++
	}
}
func (a *auditedTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	a.query(sql)
	return a.Tx.Query(ctx, sql, args...)
}
func (a *auditedTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	a.query(sql)
	return a.Tx.QueryRow(ctx, sql, args...)
}
func (a *auditedTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	a.directWrites++
	return a.Tx.Exec(ctx, sql, args...)
}
func (a *auditedTx) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	a.batches++
	return a.Tx.SendBatch(ctx, batch)
}

func TestProjectionKeepsFullResultsAndIsIdempotent(t *testing.T) {
	s := fixture(t)
	a, err := s.Accept(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := harness.Snapshot{Path: new("/history"), Offset: 10, Turns: []harness.Turn{{NativeID: "turn", Status: session.Running, Items: []harness.Item{}}}}
	if err := s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, r *SessionRecord) error {
		r.ThreadID = new("thread")
		run, err := GetRun(t.Context(), tx, a.SessionID, a.RunID)
		if err != nil {
			return err
		}
		run.NativeTurnID = new("turn")
		return SaveRun(t.Context(), tx, &run)
	}); err != nil {
		t.Fatal(err)
	}
	// Large native identity sets use array parameters, not one placeholder per ID.
	for i := range 2000 {
		snapshot.Turns[0].Items = append(snapshot.Turns[0].Items, harness.Item{NativeID: fmt.Sprint(i), Type: "tool", Index: i, Name: "commandExecution", Input: json.RawMessage(`"echo"`), Status: "completed", Result: json.RawMessage(`{"type":"text","text":"complete output","exit_code":0,"original_bytes":15}`), Completeness: "complete"})
	}
	var audit *auditedTx
	apply := func() error {
		return s.Mutate(t.Context(), a.SessionID, false, func(tx pgx.Tx, r *SessionRecord) error {
			audit = &auditedTx{Tx: tx}
			return Reconcile(t.Context(), audit, r, snapshot)
		})
	}
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	if audit.selects > 6 || audit.batches != 1 || audit.directWrites > 3 {
		t.Fatalf("large projection wasn't batched: %+v", audit)
	}
	before, _, err := s.Read(t.Context(), a.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	if audit.selects > 6 || audit.resultReads != 0 || audit.batches != 0 || audit.directWrites != 0 {
		t.Fatalf("unchanged projection reloaded output or wrote rows: %+v", audit)
	}
	after, _, err := s.Read(t.Context(), a.SessionID)
	if err != nil || before.NextEventSequence != after.NextEventSequence {
		t.Fatal("unchanged snapshot emitted events", err)
	}
	for i := range snapshot.Turns[0].Items {
		item := &snapshot.Turns[0].Items[i]
		item.Result = json.RawMessage(`{"type":"text","text":"tail","original_bytes":null}`)
		item.Completeness = "truncated"
		item.TruncationReason = new("harness_limit")
	}
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	after, _, err = s.Read(t.Context(), a.SessionID)
	if err != nil || before.NextEventSequence != after.NextEventSequence {
		t.Fatal("poorer snapshot emitted events", err)
	}
	var result string
	if err := s.Pool.QueryRow(t.Context(), "SELECT result->>'text' FROM tool_calls WHERE session_id=$1 LIMIT 1", a.SessionID).Scan(&result); err != nil || result != "complete output" {
		t.Fatal(result, err)
	}
	snapshot.Turns[0].Items[0].Name = "renamed"
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	if audit.selects > 6 || audit.resultReads != 1 || audit.batches != 1 {
		t.Fatalf("changed output wasn't fetched in one batch: %+v", audit)
	}
	after, _, err = s.Read(t.Context(), a.SessionID)
	if err != nil || after.NextEventSequence != before.NextEventSequence+1 {
		t.Fatal("one change did not produce one event", err)
	}
	// Failure after enqueueing entity/event pairs must not commit any of them
	// or advance the durable JSONL cursor.
	snapshot.Offset = 999
	snapshot.Turns[0].Items[0].Name = "rollback"
	snapshot.Turns[0].Items[1].Status = "invalid-status"
	if err := apply(); err == nil {
		t.Fatal("invalid batched projection accepted")
	}
	rolledBack, _, err := s.Read(t.Context(), a.SessionID)
	if err != nil || rolledBack.NextEventSequence != after.NextEventSequence || rolledBack.HistoryOffset != 10 {
		t.Fatal("failed batch committed partial projection", err)
	}
}
