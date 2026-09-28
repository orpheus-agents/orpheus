//go:build integration

package store

import (
	"context"
	"testing"

	"github.com/orpheus-agents/orpheus/internal/store/db"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

func TestExpiryCleanupPreservesUnexpiredRecords(t *testing.T) {
	pool := testutil.Database(t)
	queries := db.New(pool)
	for _, tc := range []struct {
		table string
		seed  string
		clean func(context.Context) (int64, error)
	}{
		{"browser_login_requests", `INSERT INTO browser_login_requests (id, request_id, browser_nonce_hash, return_path, expires_at)
		SELECT i::text, i::text, decode(repeat('00', 32), 'hex'), '/',
		 now() + CASE WHEN i = 1002 THEN interval '1 hour' ELSE interval '-1 second' END
		FROM generate_series(1, 1002) AS i`, queries.CleanupBrowserLogins},
		{"browser_sessions", `INSERT INTO browser_sessions (token_hash, subject, display_name, expires_at)
		SELECT decode(md5(i::text) || md5(i::text), 'hex'), 'user', 'User',
		 now() + CASE WHEN i = 1002 THEN interval '1 hour' ELSE interval '-1 second' END
		FROM generate_series(1, 1002) AS i`, queries.CleanupBrowserSessions},
	} {
		t.Run(tc.table, func(t *testing.T) {
			if _, err := pool.Exec(t.Context(), tc.seed); err != nil {
				t.Fatal(err)
			}
			for _, want := range []int64{1000, 1, 0} {
				if got, err := tc.clean(t.Context()); err != nil || got != want {
					t.Fatalf("want %d deleted, got %d: %v", want, got, err)
				}
			}
			var total, valid int
			if err := pool.QueryRow(t.Context(), "SELECT count(*), count(*) FILTER (WHERE expires_at > clock_timestamp()) FROM "+tc.table).Scan(&total, &valid); err != nil {
				t.Fatal(err)
			}
			if total != 1 || valid != 1 {
				t.Fatalf("unexpired record was not preserved: total=%d valid=%d", total, valid)
			}
		})
	}
}
