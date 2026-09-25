//go:build integration

package migrate_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/orpheus-agents/orpheus/internal/migrate"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

func TestUpDownUpAndConstraints(t *testing.T) {
	pool := testutil.Database(t)
	db := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer func() { _ = db.Close() }()
	p, err := migrate.Provider(db, "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	var tables int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name!='goose_db_version'").Scan(&tables); err != nil || tables != 0 {
		t.Fatal(tables, err)
	}
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sessions", "runs", "messages", "tool_calls", "session_events", "operations", "idempotency_keys", "account_limit_observations"} {
		var exists bool
		if err := pool.QueryRow(t.Context(), "SELECT to_regclass($1) IS NOT NULL", name).Scan(&exists); err != nil || !exists {
			t.Fatal(name, exists, err)
		}
	}
	for _, name := range []string{"sessions_reserved", "runs_one_unfinished_per_session", "operations_session"} {
		var exists bool
		if err := pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE schemaname=current_schema() AND indexname=$1)`, name).Scan(&exists); err != nil || !exists {
			t.Fatal("missing index", name, err)
		}
	}
	for _, name := range []string{"message_role_delivery", "run_final_message"} {
		var exists bool
		if err := pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE connamespace=current_schema()::regnamespace AND conname=$1)`, name).Scan(&exists); err != nil || !exists {
			t.Fatal("missing constraint", name, err)
		}
	}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(t.Context(), "INSERT INTO sessions(id,configuration) VALUES('00000000-0000-0000-0000-000000000001','{}')"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "INSERT INTO runs(id,session_id,number) VALUES('00000000-0000-0000-0000-000000000002','00000000-0000-0000-0000-000000000001',1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "INSERT INTO runs(id,session_id,number) VALUES('00000000-0000-0000-0000-000000000003','00000000-0000-0000-0000-000000000001',2)"); err == nil {
		t.Fatal("two active runs accepted")
	}
}
