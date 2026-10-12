//go:build integration

package migrate_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/orpheus-agents/orpheus/internal/migrate"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

func TestSandboxProvisioningMigrationPreservesUsedEnvironments(t *testing.T) {
	pool := testutil.Database(t)
	sqlDB := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer func() { _ = sqlDB.Close() }()
	provider, err := migrate.Provider(sqlDB, "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(t.Context(), 22); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"ready", "paused", "resuming", "unavailable", "resuming_before_paths"} {
		sid, rid, oid := uuid.New(), uuid.New(), uuid.New()
		beforePaths := state == "resuming_before_paths"
		if beforePaths {
			state = "resuming"
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO sessions(id,configuration,sandbox_state,sandbox_id,workspace,slot_reserved) VALUES($1,'{}',$2,'box','/workspace',false)`, sid, state); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO runs(id,session_id,number,status,execution_started_at,finished_at) VALUES($1,$2,1,'completed',now()-interval '1 hour',now())`, rid, sid); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO operations(id,session_id,kind,status,result) VALUES($1,$2,'sandbox_create','confirmed','{"sandbox_id":"box"}')`, oid, sid); err != nil {
			t.Fatal(err)
		}
		if beforePaths {
			if _, err := pool.Exec(t.Context(), `UPDATE sessions SET workspace=NULL,sandbox_last_known_state='paused' WHERE id=$1`, sid); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `UPDATE runs SET execution_started_at=NULL WHERE id=$1`, rid); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	var adopted, deadlines, reserved int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FILTER(WHERE result->>'adopted'='true'),count(deadline_at) FROM operations`).Scan(&adopted, &deadlines); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions WHERE slot_reserved`).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if adopted != 5 || deadlines != 0 || reserved != 0 {
		t.Fatal(adopted, deadlines, reserved)
	}
	if _, err := provider.DownTo(t.Context(), 22); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
}
