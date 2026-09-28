//go:build integration

package migrate_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/orpheus-agents/orpheus/internal/migrate"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

const markerMigration = "CREATE TABLE migration_marker (id integer PRIMARY KEY); INSERT INTO migration_marker VALUES (1);"

func migrationFixture(t *testing.T, up string) (*sql.DB, *pgx.ConnConfig, string) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_DATABASE_URL is required")
	}
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = admin.Close() })
	// Advisory locks are database-wide; isolate deliberate lock contention from
	// other packages migrating their own schemas in parallel.
	name := "migration_" + uuid.NewString()
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.ExecContext(t.Context(), "CREATE DATABASE "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	cfg.Database = name
	database := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = database.Close() })
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "00001_marker.sql"), []byte("-- +goose Up\n"+up+"\n-- +goose Down\nDROP TABLE migration_marker;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return database, cfg, directory
}

func holdMigrationLock(t *testing.T, database *sql.DB, id int64) func() {
	t.Helper()
	conn, err := database.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.ExecContext(t.Context(), "SELECT pg_advisory_lock($1)", id); err != nil {
		t.Fatal(err)
	}
	return sync.OnceFunc(func() {
		t.Helper()
		if _, err := conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", id); err != nil {
			t.Error(err)
		}
	})
}

func migrationDSN(t *testing.T, cfg *pgx.ConnConfig, name string) string {
	t.Helper()
	runConfig := cfg.Copy()
	runConfig.RuntimeParams["application_name"] = name
	dsn := stdlib.RegisterConnConfig(runConfig)
	t.Cleanup(func() { stdlib.UnregisterConnConfig(dsn) })
	return dsn
}

func startMigration(t *testing.T, ctx context.Context, cfg *pgx.ConnConfig, name, command, directory string) <-chan error {
	t.Helper()
	dsn := migrationDSN(t, cfg, name)
	done := make(chan error, 1)
	go func() { done <- migrate.Run(ctx, dsn, command, directory) }()
	return done
}

func waitMigrationQuery(t *testing.T, database *sql.DB, name, pattern string, done <-chan error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		var found bool
		if err := database.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND query LIKE $2)`, name, pattern).Scan(&found); err != nil {
			t.Fatal(err)
		}
		if found {
			return
		}
		select {
		case err := <-done:
			t.Fatal("migration returned before reaching expected lock", err)
		case <-ctx.Done():
			t.Fatal("migration did not reach expected lock", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func migrationResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("migration did not finish")
		return nil
	}
}

func TestMigrationCommandsWaitForLockAndCancel(t *testing.T) {
	for _, command := range []string{"up", "down", "reset"} {
		t.Run(command, func(t *testing.T) {
			database, cfg, directory := migrationFixture(t, markerMigration)
			if command != "up" {
				if err := migrate.Run(t.Context(), migrationDSN(t, cfg, uuid.NewString()), "up", directory); err != nil {
					t.Fatal(err)
				}
			}
			unlock := holdMigrationLock(t, database, lock.DefaultLockID)
			defer unlock()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			name := uuid.NewString()
			done := startMigration(t, ctx, cfg, name, command, directory)
			waitMigrationQuery(t, database, name, "SELECT pg_try_advisory_lock%", done)
			cancel()
			err := migrationResult(t, done)
			if !errors.Is(err, context.Canceled) {
				t.Fatal("lock wait did not report cancellation", err)
			}
			if _, ok := errors.AsType[*migrate.LockError](err); !ok {
				t.Fatal("lock wait lost its error classification", err)
			}
			var marker bool
			if err := database.QueryRowContext(t.Context(), "SELECT to_regclass('migration_marker') IS NOT NULL").Scan(&marker); err != nil {
				t.Fatal(err)
			}
			if marker != (command != "up") {
				t.Fatal("waiting migration modified application schema", marker)
			}
			// Goose can initialize version zero before acquiring the migration lock.
			var version int
			if err := database.QueryRowContext(t.Context(), "SELECT max(version_id) FROM goose_db_version").Scan(&version); err != nil {
				t.Fatal(err)
			}
			want := 0
			if command != "up" {
				want = 1
			}
			if version != want {
				t.Fatal("waiting migration changed applied versions", version)
			}
		})
	}
}

func TestConcurrentMigrationsSerialize(t *testing.T) {
	const gateID int64 = 723819002
	database, cfg, directory := migrationFixture(t, fmt.Sprintf("SELECT pg_advisory_xact_lock(%d);\n%s", gateID, markerMigration))
	unlock := holdMigrationLock(t, database, gateID)
	defer unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	firstName, secondName := uuid.NewString(), uuid.NewString()
	first := startMigration(t, ctx, cfg, firstName, "up", directory)
	waitMigrationQuery(t, database, firstName, "SELECT pg_advisory_xact_lock%", first)
	second := startMigration(t, ctx, cfg, secondName, "up", directory)
	waitMigrationQuery(t, database, secondName, "SELECT pg_try_advisory_lock%", second)
	select {
	case err := <-second:
		t.Fatal("second migration bypassed the owner", err)
	default:
	}
	unlock()
	if err := migrationResult(t, first); err != nil {
		t.Fatal("first migration", err)
	}
	if err := migrationResult(t, second); err != nil {
		t.Fatal("waiting migration", err)
	}
	var rows int
	if err := database.QueryRowContext(t.Context(), "SELECT count(*) FROM migration_marker").Scan(&rows); err != nil || rows != 1 {
		t.Fatal("migration was not applied exactly once", rows, err)
	}
}

func TestMigrationUnlocksAfterFailure(t *testing.T) {
	database, _, directory := migrationFixture(t, "SELECT 1 / 0;")
	// Keep the physical connection alive so closing the pool cannot mask a leaked lock.
	database.SetMaxOpenConns(1)
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		t.Fatal(err)
	}
	provider, err := migrate.Provider(database, directory, goose.WithSessionLocker(locker))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err == nil {
		t.Fatal("invalid migration succeeded")
	} else if pgerr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgerr.Code != "22012" {
		t.Fatal("expected division by zero in migration", err)
	}
	var locked bool
	if err := database.QueryRowContext(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE pid=pg_backend_pid() AND locktype='advisory' AND classid=($1::bigint >> 32)::oid AND objid=($1::bigint & 4294967295)::oid AND granted)`, lock.DefaultLockID).Scan(&locked); err != nil || locked {
		t.Fatal("migration failure leaked the session lock", locked, err)
	}
}

func TestProviderDoesNotLockIsolatedTestSchemas(t *testing.T) {
	database, _, directory := migrationFixture(t, markerMigration)
	unlock := holdMigrationLock(t, database, lock.DefaultLockID)
	defer unlock()
	provider, err := migrate.Provider(database, directory)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal("plain Provider contends for the CLI migration lock", err)
	}
}
