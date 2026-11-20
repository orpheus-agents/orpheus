//go:build integration

package migrate_test

import (
	"testing"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/orpheus-agents/orpheus/internal/migrate"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

func TestHarnessProvidersUpDown(t *testing.T) {
	ctx := t.Context()
	pool := testutil.DatabaseAt(t, 31)
	db := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer func() { _ = db.Close() }()
	m, err := migrate.Provider(db, "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	const legacy = "00000000-0000-0000-0000-000000000001"
	if _, err := pool.Exec(ctx, "INSERT INTO llm_accounts(id,name,harness,auth_mode,credential_store,credential_key) VALUES($1,'legacy','codex','account','store','account.json')", legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := m.UpTo(ctx, 32); err != nil {
		t.Fatal(err)
	}
	var provider string
	if err := pool.QueryRow(ctx, "SELECT provider FROM llm_accounts WHERE id=$1", legacy).Scan(&provider); err != nil || provider != "openai" {
		t.Fatal(provider, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE llm_accounts SET harness='opencode', provider='opencode' WHERE id=$1", legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Down(ctx); err == nil {
		t.Fatal("rollback discarded an OpenCode account")
	}
	if _, err := pool.Exec(ctx, "UPDATE llm_accounts SET harness='codex', provider='openai' WHERE id=$1", legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Down(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.UpTo(ctx, 32); err != nil {
		t.Fatal(err)
	}
}
