//go:build integration

package migrate_test

import (
	"testing"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/orpheus-agents/orpheus/internal/migrate"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

func TestOpenCodeVariantMigration(t *testing.T) {
	ctx := t.Context()
	pool := testutil.DatabaseAt(t, 32)
	db := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer func() { _ = db.Close() }()
	m, err := migrate.Provider(db, "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, up := range []bool{true, false, true} {
		if up {
			_, err = m.UpTo(ctx, 33)
		} else {
			_, err = m.Down(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='model_profiles' AND column_name='opencode_variant' AND is_nullable='YES')`).Scan(&exists); err != nil || exists != up {
			t.Fatal(up, exists, err)
		}
	}
}
