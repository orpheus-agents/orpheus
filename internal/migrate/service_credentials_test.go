//go:build integration

package migrate_test

import (
	"testing"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/orpheus-agents/orpheus/internal/migrate"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

func TestServiceCredentialMigrationUpDownUp(t *testing.T) {
	pool := testutil.Database(t)
	db := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer func() { _ = db.Close() }()
	p, err := migrate.Provider(db, "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err = p.DownTo(t.Context(), 24); err != nil {
			t.Fatal(err)
		}
		var missing bool
		if err = pool.QueryRow(t.Context(), `SELECT to_regclass('run_service_credentials') IS NULL AND to_regclass('run_input_subjects') IS NULL`).Scan(&missing); err != nil || !missing {
			t.Fatal("Down retained tables", err)
		}
		if _, err = p.Up(t.Context()); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"run_service_credentials", "run_input_subjects"} {
			var present bool
			if err = pool.QueryRow(t.Context(), `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&present); err != nil || !present {
				t.Fatal(table, err)
			}
		}
	}
}
