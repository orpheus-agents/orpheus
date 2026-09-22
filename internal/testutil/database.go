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
	"github.com/skillum-ai/orpheus/internal/migrate"
)

func Database(t *testing.T) *pgxpool.Pool {
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
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = name
	db := stdlib.OpenDB(*cfg.ConnConfig)
	t.Cleanup(func() { _ = db.Close() })
	Migrate(t, db)
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
func Migrate(t *testing.T, db *sql.DB) {
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
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
}
