//go:build integration

package sqlcheck

import (
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/orpheus-agents/orpheus/internal/testutil"
)

// sqlc's prepare rule asks PostgreSQL to validate every query without executing
// it. Use the real Goose schema, isolated from other tests and application data.
func TestQueriesPrepareAgainstGooseSchema(t *testing.T) {
	pool := testutil.Database(t)
	uri, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	params := uri.Query()
	params.Set("search_path", pool.Config().ConnConfig.RuntimeParams["search_path"])
	uri.RawQuery = params.Encode()
	t.Setenv("SQLC_DATABASE_URL", uri.String())
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	vet := func() ([]byte, error) {
		command := exec.CommandContext(t.Context(), "sqlc", "vet")
		command.Dir = root
		return command.CombinedOutput()
	}
	if output, err := vet(); err != nil {
		t.Fatalf("sqlc vet: %v\n%s", err, output)
	}
	var legacyColumns int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name IN ('sessions','runs') AND column_name='env_ciphertext'`).Scan(&legacyColumns); err != nil || legacyColumns != 0 {
		t.Fatal("fresh installation retained legacy columns", legacyColumns, err)
	}
	// Prove the configured rule actually consults PostgreSQL rather than merely
	// accepting the SQL parsed from the migration files.
	if _, err := pool.Exec(t.Context(), "ALTER TABLE sessions DROP COLUMN sandbox_state"); err != nil {
		t.Fatal(err)
	}
	if output, err := vet(); err == nil {
		t.Fatalf("sqlc vet accepted schema drift: %s", output)
	}
}
