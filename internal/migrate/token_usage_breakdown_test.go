//go:build integration

package migrate_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/orpheus-agents/orpheus/internal/migrate"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

func TestTokenUsageBreakdownMigrations(t *testing.T) {
	pool := testutil.Database(t)
	db := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer func() { _ = db.Close() }()
	p, err := migrate.Provider(db, "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(t.Context(), 15); err != nil {
		t.Fatal(err)
	}
	oldSID, oldRID := uuid.New(), uuid.New()
	if _, err := pool.Exec(t.Context(), `INSERT INTO sessions(id,configuration) VALUES($1,'{}')`, oldSID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO runs(id,session_id,number,status,input_tokens,output_tokens,total_tokens) VALUES($1,$2,1,'completed',90,10,100)`, oldRID, oldSID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO session_events(session_id,sequence,type,data) VALUES($1,1,'run.updated','{"usage":{"input_tokens":90,"output_tokens":10,"total_tokens":100}}')`, oldSID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	var cached, reasoning int64
	var nativeCached, nativeReasoning *int64
	if err := pool.QueryRow(t.Context(), `SELECT cached_input_tokens,reasoning_output_tokens,cached_input_native_total,reasoning_output_native_total FROM sessions WHERE id=$1`, oldSID).Scan(&cached, &reasoning, &nativeCached, &nativeReasoning); err != nil || cached != 0 || reasoning != 0 || nativeCached != nil || nativeReasoning != nil {
		t.Fatal(cached, reasoning, nativeCached, nativeReasoning, err)
	}
	newSID, newRID := uuid.New(), uuid.New()
	if _, err := pool.Exec(t.Context(), `INSERT INTO sessions(id,configuration) VALUES($1,'{}')`, newSID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO runs(id,session_id,number,status) VALUES($1,$2,1,'accepted')`, newRID, newSID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT cached_input_tokens,reasoning_output_tokens,cached_input_native_total,reasoning_output_native_total FROM sessions WHERE id=$1`, newSID).Scan(&cached, &reasoning, &nativeCached, &nativeReasoning); err != nil || cached != 0 || reasoning != 0 || nativeCached == nil || *nativeCached != 0 || nativeReasoning == nil || *nativeReasoning != 0 {
		t.Fatal(cached, reasoning, nativeCached, nativeReasoning, err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT cached_input_tokens,reasoning_output_tokens FROM runs WHERE id=$1`, oldRID).Scan(&cached, &reasoning); err != nil || cached != 0 || reasoning != 0 {
		t.Fatal(cached, reasoning, err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE runs SET cached_input_tokens=-1 WHERE id=$1`, newRID); err == nil {
		t.Fatal("negative cached usage accepted")
	}
	if _, err := pool.Exec(t.Context(), `UPDATE sessions SET reasoning_output_native_total=-1 WHERE id=$1`, newSID); err == nil {
		t.Fatal("negative native usage accepted")
	}
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	var eventCached, eventReasoning int64
	if err := pool.QueryRow(t.Context(), `SELECT (data#>>'{usage,cached_input_tokens}')::bigint,(data#>>'{usage,reasoning_output_tokens}')::bigint FROM session_events WHERE session_id=$1`, oldSID).Scan(&eventCached, &eventReasoning); err != nil || eventCached != 0 || eventReasoning != 0 {
		t.Fatal(eventCached, eventReasoning, err)
	}
	if _, err := p.DownTo(t.Context(), 15); err != nil {
		t.Fatal(err)
	}
	var unchanged bool
	if err := pool.QueryRow(t.Context(), `SELECT data->'usage' = '{"input_tokens":90,"output_tokens":10,"total_tokens":100}'::jsonb FROM session_events WHERE session_id=$1`, oldSID).Scan(&unchanged); err != nil || !unchanged {
		t.Fatal(unchanged, err)
	}
}
