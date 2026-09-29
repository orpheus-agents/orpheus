//go:build integration

package migrate_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/orpheus-agents/orpheus/internal/migrate"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

func TestSingleRunMigration(t *testing.T) {
	pool := testutil.Database(t)
	db := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer func() { _ = db.Close() }()
	p, err := migrate.Provider(db, "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(t.Context(), 17); err != nil {
		t.Fatal(err)
	}
	old := uuid.New()
	if _, err := pool.Exec(t.Context(), `INSERT INTO sessions(id,configuration,sandbox_state,sandbox_id) VALUES($1,'{}','paused','retained')`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	var multiple bool
	var state, sandboxID string
	if err := pool.QueryRow(t.Context(), `SELECT allow_multiple_runs,sandbox_state,sandbox_id FROM sessions WHERE id=$1`, old).Scan(&multiple, &state, &sandboxID); err != nil || !multiple || state != "paused" || sandboxID != "retained" {
		t.Fatal(multiple, state, sandboxID, err)
	}
	for _, state := range []string{"deleting", "deleted"} {
		id := uuid.New()
		if err := pool.QueryRow(t.Context(), `INSERT INTO sessions(id,configuration,sandbox_state,sandbox_last_known_state,sandbox_id) VALUES($1,'{}',$2,'deleted','diagnostic-id') RETURNING allow_multiple_runs`, id, state).Scan(&multiple); err != nil || multiple {
			t.Fatal("default", multiple, err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO session_events(session_id,sequence,type,data) VALUES($1,1,'sandbox.updated',jsonb_build_object('state',$2::text,'last_known_state','deleted','id','diagnostic-id'))`, id, state); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO sessions(id,configuration,allow_multiple_runs) VALUES($1,'{}',NULL)`, uuid.New()); err == nil {
		t.Fatal("nullable policy")
	}
	if _, err := p.DownTo(t.Context(), 17); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions WHERE sandbox_state='unavailable' AND sandbox_last_known_state='unavailable' AND sandbox_id='diagnostic-id'`).Scan(&count); err != nil || count != 2 {
		t.Fatal("rollback lost diagnostic data", count, err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM session_events WHERE data->>'state'='unavailable' AND data->>'last_known_state'='unavailable' AND data->>'id'='diagnostic-id'`).Scan(&count); err != nil || count != 2 {
		t.Fatal("rollback left unsupported events", count, err)
	}
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions WHERE allow_multiple_runs`).Scan(&count); err != nil || count != 3 {
		t.Fatal("up/down/up", count, err)
	}
}
