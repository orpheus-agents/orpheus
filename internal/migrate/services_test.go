//go:build integration

package migrate_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/orpheus-agents/orpheus/internal/migrate"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

func TestServiceMigrations(t *testing.T) {
	pool := testutil.DatabaseAt(t, 29)
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
		if _, err := p.UpTo(t.Context(), 29); err != nil {
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

func TestDirectServiceMigrationUsesEachHistoricalSnapshot(t *testing.T) {
	pool := testutil.DatabaseAt(t, 33)
	ctx := t.Context()
	db := stdlib.OpenDB(*pool.Config().ConnConfig)
	t.Cleanup(func() { _ = db.Close() })
	provider, err := migrate.Provider(db, "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	sid, rid, hid := uuid.New(), uuid.New(), uuid.New()
	for _, item := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO sessions(id,configuration) VALUES($1,'{"version":1,"public":{"sandbox":{"template":"box","services":[{"code":"b","env_from":["B"]},{"code":"a","env_from":["A","SHARED"]}],"env_from":["Z","SHARED","B","A"],"env_names":["VALUE"]}}}')`, []any{sid}},
		{`INSERT INTO runs(id,session_id,number,services,env_from,env_names) VALUES($1,$2,1,'[{"code":"b","env_from":["OLD_B"]}]',ARRAY['M','OLD_B'],ARRAY['INPUT'])`, []any{rid, sid}},
		{`INSERT INTO hook_executions(id,session_id,run_id,name,status) VALUES($1,$2,$3,'before_run','completed')`, []any{hid, sid, rid}},
		{`INSERT INTO session_events(session_id,sequence,type,data) VALUES($1,1,'run.updated','{"services":[{"code":"retired","env_from":["EVENT"]}],"env_from":["EVENT","KEEP"],"hooks":[{"name":"before_run"},{"name":"after_run"}],"env_names":["VALUE"]}'),($1,2,'message.updated','{"untouched":true}')`, []any{sid}},
	} {
		if _, err := pool.Exec(ctx, item.query, item.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := provider.UpTo(ctx, 35); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`SELECT configuration = '{"version":1,"public":{"sandbox":{"template":"box","services":["a","b"],"env_from":["Z"],"env_names":["VALUE"]}}}'::jsonb FROM sessions`,
		`SELECT services='["b"]' AND env_from=ARRAY['M'] AND env_names=ARRAY['INPUT'] AND harness_launch_id IS NULL FROM runs`,
		`SELECT started_attempt IS NULL AND attempt_env_names='[]'::jsonb FROM hook_executions`,
		`SELECT data='{"services":["retired"],"env_from":["KEEP"],"hooks":[{"name":"before_run","env_names":null},{"name":"after_run","env_names":null}],"env_names":["VALUE"],"harness_env_names":null}'::jsonb FROM session_events WHERE sequence=1`,
		`SELECT data='{"untouched":true}'::jsonb FROM session_events WHERE sequence=2`,
	} {
		var ok bool
		if err := pool.QueryRow(ctx, q).Scan(&ok); err != nil || !ok {
			t.Fatal(q, err)
		}
	}
	if _, err := provider.DownTo(ctx, 33); err == nil || !strings.Contains(err.Error(), "pre-upgrade database backup") {
		t.Fatal("expected backup requirement", err)
	}
	// A database with no selections can reverse both migrations and apply them again.
	if _, err := pool.Exec(ctx, `UPDATE sessions SET configuration=jsonb_set(configuration,'{public,sandbox,services}','[]'); UPDATE runs SET services='[]'; UPDATE session_events SET data=jsonb_set(data,'{services}','[]') WHERE type='run.updated'`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 33); err != nil {
		t.Fatal(err)
	}
	var removed bool
	if err := pool.QueryRow(ctx, `SELECT NOT (data ? 'harness_env_names' OR (data->'hooks'->0) ? 'env_names') FROM session_events WHERE sequence=1`).Scan(&removed); err != nil || !removed {
		t.Fatal(removed, err)
	}
	if _, err := provider.UpTo(ctx, 35); err != nil {
		t.Fatal(err)
	}
}
