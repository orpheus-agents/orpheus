//go:build integration

package config

import (
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDatabaseCallTimeouts(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_DATABASE_URL is required")
	}
	cfg, err := DatabasePoolConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.Tracer = &databaseTimeoutTracer{timeout: 50 * time.Millisecond}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = pool.Acquire(t.Context())
	conn.Release()
	if err == nil || time.Since(started) > time.Second {
		t.Fatal("pool acquisition was not bounded", err)
	}

	started = time.Now()
	var value int
	err = pool.QueryRow(t.Context(), "SELECT 1 FROM pg_sleep(5)").Scan(&value)
	if err == nil || time.Since(started) > time.Second {
		t.Fatal("database query was not bounded", err)
	}
}
