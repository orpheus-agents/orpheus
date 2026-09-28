//go:build integration

package migrate

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/pressly/goose/v3/lock"
)

func TestSessionLockerClassifiesExhaustedRetries(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_DATABASE_URL is required")
	}
	database, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	holder, err := database.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	const lockID int64 = 723819003
	if _, err := holder.ExecContext(t.Context(), "SELECT pg_advisory_lock($1)", lockID); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = holder.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", lockID) }()
	waiter, err := database.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = waiter.Close() }()
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockID(lockID), lock.WithLockTimeout(1, 1))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err = (sessionLocker{locker}).SessionLock(ctx, waiter)
	if ctx.Err() != nil {
		t.Fatal("expected Goose retry exhaustion, not context cancellation", ctx.Err())
	}
	lockErr, ok := errors.AsType[*LockError](err)
	if !ok || errors.Unwrap(lockErr) == nil {
		t.Fatal("lock exhaustion lost its classification or cause", err)
	}
}
