//go:build integration

package migrate_test

import (
	"testing"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/orpheus-agents/orpheus/internal/migrate"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

func TestBrowserAuthMigrationUpDown(t *testing.T) {
	pool := testutil.Database(t)
	sqlDB := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer func() { _ = sqlDB.Close() }()
	provider, err := migrate.Provider(sqlDB, "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"browser_sessions", "browser_login_requests"} {
		var exists bool
		if err := pool.QueryRow(t.Context(), "SELECT to_regclass($1) IS NOT NULL", name).Scan(&exists); err != nil || !exists {
			t.Fatal(name, err)
		}
	}
	if _, err := provider.DownTo(t.Context(), 12); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"browser_sessions", "browser_login_requests"} {
		var exists bool
		if err := pool.QueryRow(t.Context(), "SELECT to_regclass($1) IS NOT NULL", name).Scan(&exists); err != nil || exists {
			t.Fatal(name, err)
		}
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), "INSERT INTO browser_sessions(token_hash,subject,display_name,expires_at) VALUES ('short','s','d',now())"); err == nil {
		t.Fatal("short token hash accepted")
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO browser_login_requests(id,request_id,browser_nonce_hash,return_path,expires_at) VALUES ('pending','request',decode(repeat('00',32),'hex'),'/',now()+interval '10 minutes')`); err != nil {
		t.Fatal(err)
	}
	var previous []byte
	if err := pool.QueryRow(t.Context(), "SELECT previous_session_hash FROM browser_login_requests WHERE id='pending'").Scan(&previous); err != nil || previous != nil {
		t.Fatal(previous, err)
	}
	if _, err := pool.Exec(t.Context(), "UPDATE browser_login_requests SET previous_session_hash='short'"); err == nil {
		t.Fatal("invalid previous session hash accepted")
	}
	if _, err := pool.Exec(t.Context(), "UPDATE browser_login_requests SET previous_session_hash=decode(repeat('01',32),'hex')"); err != nil {
		t.Fatal(err)
	}

}
