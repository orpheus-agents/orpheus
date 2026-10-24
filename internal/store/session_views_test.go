//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

type sessionQueryCounter struct{ count atomic.Int64 }

func (c *sessionQueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "-- name:") {
		c.count.Add(1)
	}
	return ctx
}
func (*sessionQueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestSessionViewsBatch(t *testing.T) {
	s := fixture(t)
	ctx := t.Context()
	statuses := []session.Status{session.Accepted, session.Starting, session.Running, session.Cancelling, session.Finalizing, session.Completed, session.Failed, session.Cancelled}
	var records []SessionRecord
	var expected []session.Session
	for i := range 60 {
		sid, rid, mid := uuid.New(), uuid.New(), uuid.New()
		status := statuses[i%len(statuses)]
		for _, stmt := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO sessions(id,configuration,namespace,external_key,total_tokens) VALUES($1,'{}','batch',$2,$3)`, []any{sid, fmt.Sprint(i), i + 1}},
			// Latest means greatest run number, not greatest timestamp.
			{`INSERT INTO runs(id,session_id,number,status,created_at) VALUES($1,$1,1,'completed',now()+interval '1 day')`, []any{sid}},
			{`INSERT INTO runs(id,session_id,number,status,phase,error) VALUES($1,$2,2,$3,'after_run','{"code":"fixture","message":"test"}')`, []any{rid, sid, status}},
			{`INSERT INTO messages(id,session_id,run_id,role,kind,text,registered_sequence) VALUES($1,$2,$3,'assistant','answer',$4,1)`, []any{mid, sid, rid, fmt.Sprintf("answer %d", i)}},
			{`UPDATE runs SET final_message_id=$1 WHERE id=$2`, []any{mid, rid}},
			{`INSERT INTO hook_executions(id,session_id,run_id,name,status) VALUES($1,$2,$3,'after_run','completed')`, []any{uuid.New(), sid, rid}},
		} {
			if _, err := s.Pool.Exec(ctx, stmt.sql, stmt.args...); err != nil {
				t.Fatal(err)
			}
		}
		record, err := GetSession(ctx, s.Pool, sid, false)
		if err != nil {
			t.Fatal(err)
		}
		run, err := GetRun(ctx, s.Pool, sid, rid)
		if err != nil {
			t.Fatal(err)
		}
		want := session.Session{AllowMultipleRuns: record.AllowMultipleRuns, Usage: record.Usage(), Namespace: record.Namespace, ExternalKey: record.ExternalKey, ID: sid, CreatedAt: record.CreatedAt, Configuration: record.Configuration.Public, Sandbox: record.Sandbox(), LastRunID: rid, LastRunCreatedAt: run.CreatedAt, Status: status, Phase: run.Phase, Error: run.Error}
		if !status.Terminal() {
			want.ActiveRunID = new(rid)
		}
		if status.Terminal() || status == session.Finalizing {
			message, err := GetMessage(ctx, s.Pool, mid)
			if err != nil {
				t.Fatal(err)
			}
			want.FinalMessage = &message.Message
		}
		records = append(records, record)
		expected = append(expected, want)
	}
	// Deliberately use an order unrelated to UUIDs or run timestamps.
	slices.Reverse(records)
	slices.Reverse(expected)
	views, err := SessionViews(ctx, s.Pool, records)
	if err != nil || !reflect.DeepEqual(views, expected) {
		t.Fatalf("batch projections differ: err=%v", err)
	}
	for i, record := range records {
		view, err := SessionView(ctx, s.Pool, record)
		if err != nil || !reflect.DeepEqual(view, expected[i]) {
			t.Fatalf("single projection %d: %v", i, err)
		}
	}
	// Trace the public list path, so reverting to per-session loading fails this test.
	counter := &sessionQueryCounter{}
	cfg := s.Pool.Config()
	cfg.ConnConfig.Tracer = counter
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s.Pool = pool
	for _, limit := range []int{10, 50} {
		counter.count.Store(0)
		page, err := s.ListSessions(ctx, limit, "", ListFilter{Sort: "last_run_created_at", Order: "desc"})
		if err != nil || len(page.Items) != limit || page.NextCursor == nil {
			t.Fatalf("page %d: %v", limit, err)
		}
		if n := counter.count.Load(); n != 4 {
			t.Fatalf("page %d used %d queries, want 4", limit, n)
		}
		for _, item := range page.Items {
			i := slices.IndexFunc(expected, func(w session.Session) bool { return w.ID == item.ID })
			if i < 0 || !reflect.DeepEqual(item, expected[i]) {
				t.Fatal("list projection differs from expected")
			}
		}
	}
	counter.count.Store(0)
	page, err := s.ListSessions(ctx, 50, "", ListFilter{Namespace: new("missing")})
	if err != nil || page.Items == nil || len(page.Items) != 0 || page.NextCursor != nil || counter.count.Load() != 1 {
		t.Fatalf("empty page: %+v, err=%v, queries=%d", page, err, counter.count.Load())
	}
}

type failSessionQuery struct {
	db.DBTX
	name string
	err  error
}

func (q failSessionQuery) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "-- name: "+q.name+" ") {
		return nil, q.err
	}
	return q.DBTX.Query(ctx, sql, args...)
}

func TestSessionViewsEmptyMissingAndErrors(t *testing.T) {
	views, err := SessionViews(t.Context(), nil, nil)
	if err != nil || views == nil || len(views) != 0 {
		t.Fatal(views, err)
	}
	s := fixture(t)
	sid, rid, mid := uuid.New(), uuid.New(), uuid.New()
	ctx := t.Context()
	if _, err := s.Pool.Exec(ctx, `INSERT INTO sessions(id,configuration) VALUES($1,'{}')`, sid); err != nil {
		t.Fatal(err)
	}
	record, err := GetSession(ctx, s.Pool, sid, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SessionViews(ctx, s.Pool, []SessionRecord{record}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("missing run", err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO runs(id,session_id,number,status) VALUES($1,$2,1,'completed')`, rid, sid); err != nil {
		t.Fatal(err)
	}
	views, err = SessionViews(ctx, s.Pool, []SessionRecord{record})
	if err != nil || len(views) != 1 || views[0].FinalMessage != nil {
		t.Fatal("run without final message", views, err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO messages(id,session_id,run_id,role,kind,text,registered_sequence) VALUES($1,$2,$3,'assistant','answer','answer',1)`, mid, sid, rid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE runs SET final_message_id=$1 WHERE id=$2`, mid, rid); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"LatestRunsForSessions", "HookExecutionsForRuns", "MessagesByIDs"} {
		t.Run(name, func(t *testing.T) {
			want := errors.New("query failed")
			_, err := SessionViews(ctx, failSessionQuery{DBTX: s.Pool, name: name, err: want}, []SessionRecord{record})
			if !errors.Is(err, want) {
				t.Fatal("lost query error", err)
			}
		})
	}
}
