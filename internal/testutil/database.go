//go:build integration || live

// Package testutil supplies isolated PostgreSQL schemas created through Goose.
package testutil

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/migrate"
)

func Database(t *testing.T) *pgxpool.Pool {
	return DatabaseAt(t, 0)
}

// DatabaseAt prepares an isolated historical schema without reversing later
// irreversible imports. Zero selects all shipped migrations.
func DatabaseAt(t *testing.T, version int64) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_DATABASE_URL is required for integration tests")
	}
	admin, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	name := "test_" + uuid.New().String()
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE")
		_ = admin.Close(context.Background())
	})
	cfg, err := config.DatabasePoolConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = name
	db := stdlib.OpenDB(*cfg.ConnConfig)
	t.Cleanup(func() { _ = db.Close() })
	MigrateTo(t, db, version)
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
func MigrateTo(t *testing.T, db *sql.DB, version int64) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate migration files")
	}
	directory := filepath.Join(filepath.Dir(file), "..", "..", "migrations")
	p, err := migrate.Provider(db, directory)
	if err != nil {
		t.Fatal(err)
	}
	var errUp error
	if version == 0 {
		_, errUp = p.Up(t.Context())
	} else {
		_, errUp = p.UpTo(t.Context(), version)
	}
	if errUp != nil {
		t.Fatal(errUp)
	}
}
