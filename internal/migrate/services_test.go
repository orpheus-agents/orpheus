//go:build integration

package migrate_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/orpheus-agents/orpheus/internal/migrate"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

func TestServiceMigrations(t *testing.T) {
	pool := testutil.Database(t)
	db := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer func() { _ = db.Close() }()
	p, err := migrate.Provider(db, "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(t.Context(), 20); err != nil {
		t.Fatal(err)
	}
	sid, rid := uuid.New(), uuid.New()
	if _, err := pool.Exec(t.Context(), `INSERT INTO sessions(id,configuration) VALUES($1,'{"version":1,"public":{"sandbox":{"template":"codex","env_from":["OLD"],"env_names":[]}}}')`, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO runs(id,session_id,number,env_from) VALUES($1,$2,1,ARRAY['OLD'])`, rid, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO session_events(session_id,sequence,type,data) VALUES($1,1,'run.updated','{"status":"accepted","env_from":["OLD"]}')`, sid); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := p.Up(t.Context()); err != nil {
			t.Fatal(err)
		}
		var snapshot, services, eventServices, oldEnv string
		if err := pool.QueryRow(t.Context(), `SELECT configuration #>> '{public,sandbox,services}', configuration #>> '{public,sandbox,env_from,0}' FROM sessions WHERE id=$1`, sid).Scan(&snapshot, &oldEnv); err != nil || snapshot != "[]" || oldEnv != "OLD" {
			t.Fatal(snapshot, oldEnv, err)
		}
		if err := pool.QueryRow(t.Context(), `SELECT services::text FROM runs WHERE id=$1`, rid).Scan(&services); err != nil || services != "[]" {
			t.Fatal(services, err)
		}
		if err := pool.QueryRow(t.Context(), `SELECT data->>'services' FROM session_events WHERE session_id=$1 AND sequence=1`, sid).Scan(&eventServices); err != nil || eventServices != "[]" {
			t.Fatal(eventServices, err)
		}
		if _, err := p.DownTo(t.Context(), 20); err != nil {
			t.Fatal(err)
		}
		var present bool
		if err := pool.QueryRow(t.Context(), `SELECT (configuration #> '{public,sandbox}') ? 'services' FROM sessions WHERE id=$1`, sid).Scan(&present); err != nil || present {
			t.Fatal(present, err)
		}
		if err := pool.QueryRow(t.Context(), `SELECT data ? 'services' FROM session_events WHERE session_id=$1`, sid).Scan(&present); err != nil || present {
			t.Fatal(present, err)
		}
		if err := pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='runs' AND column_name='services')`).Scan(&present); err != nil || present {
			t.Fatal(present, err)
		}
	}
}
