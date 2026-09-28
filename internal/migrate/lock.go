package migrate

import (
	"context"
	"database/sql"

	"github.com/pressly/goose/v3/lock"
)

// LockError identifies a failed acquisition without assuming contention caused it.
type LockError struct {
	Err error
}

func (e *LockError) Error() string { return "cannot acquire migration lock" }
func (e *LockError) Unwrap() error { return e.Err }

type sessionLocker struct {
	lock.SessionLocker
}

func (l sessionLocker) SessionLock(ctx context.Context, conn *sql.Conn) error {
	if err := l.SessionLocker.SessionLock(ctx, conn); err != nil {
		return &LockError{Err: err}
	}
	return nil
}
