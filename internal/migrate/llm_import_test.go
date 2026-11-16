//go:build integration

package migrate_test

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/migrate"
	"github.com/orpheus-agents/orpheus/internal/secret"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

const llmTOML = `[credential_stores.s]
bucket='b'
[profiles.current]
harness='codex'
model='model'
instructions='unchanged'
[profiles.current.codex]
effort='high'
[profiles.current.auth]
mode='account'
account_id='current-account'
store='s'
key='current-auth.json'
`

func migrationInput(t *testing.T, source string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.toml")
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORPHEUS_MIGRATION_CONFIG_FILE", path)
	return path
}
func TestLLMEmptyInstallAndReplayWithoutInput(t *testing.T) {
	t.Setenv("ORPHEUS_MIGRATION_CONFIG_FILE", "")
	pool := testutil.DatabaseAt(t, 29)
	db := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer func() { _ = db.Close() }()
	p, err := migrate.Provider(db, "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := p.Up(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM llm_accounts").Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	if _, err := p.Down(t.Context()); err == nil {
		t.Fatal("irreversible import accepted Down")
	}
}
func TestLLMImportPreservesSourcesAndClosedHistory(t *testing.T) {
	pool := testutil.DatabaseAt(t, 29)
	path := migrationInput(t, llmTOML)
	db := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer func() { _ = db.Close() }()
	p, err := migrate.Provider(db, "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	type fixture struct {
		id                uuid.UUID
		key, state, oldID string
		wantAccount       bool
	}
	fixtures := []fixture{
		{uuid.New(), "current-auth.json", "paused", "renamed-account", true},
		{uuid.New(), "current-auth.json", "deleted", "renamed-account", true},
		{uuid.New(), "historical.json", "paused", "historical-account", true},
		{uuid.New(), "historical.json", "deleted", "historical-account", true},
		{uuid.New(), "", "deleted", "lost-account", false},
	}
	for _, f := range fixtures {
		credentials := any(map[string]any{"mode": "account", "account_id": f.oldID, "key": f.key, "store": map[string]any{"type": "s3", "region": "us-east-1", "bucket": "b", "endpoint_url": nil}})
		if f.key == "" {
			credentials = nil
		}
		raw, _ := json.Marshal(map[string]any{"version": 1, "harness": "codex", "public": map[string]any{"agent": map[string]any{"profile": "deleted-profile", "model": "frozen-model"}}, "credentials": credentials})
		if _, err := pool.Exec(t.Context(), "INSERT INTO sessions(id,configuration,sandbox_state,slot_reserved) VALUES($1,$2,$3,false)", f.id, raw, f.state); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), "INSERT INTO runs(id,session_id,number,status) VALUES($1,$2,1,'completed')", uuid.New(), f.id); err != nil {
			t.Fatal(err)
		}
	}
	fingerprint, err := config.SourceFingerprint("codex", session.Credentials{Mode: "account", Store: &session.CredentialStore{Type: "s3", Region: "us-east-1", Bucket: "b"}, Key: "current-auth.json"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO account_limit_observations(account_id,source_fingerprint,last_attempt_at,last_error_code) VALUES('renamed-account',$1,'2026-11-01T00:00:00Z','no_data')`, fingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM llm_accounts").Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	for _, f := range fixtures {
		var id *uuid.UUID
		var cfg []byte
		if err := pool.QueryRow(t.Context(), "SELECT llm_account_id,configuration FROM sessions WHERE id=$1", f.id).Scan(&id, &cfg); err != nil {
			t.Fatal(err)
		}
		if (id != nil) != f.wantAccount || strings.Contains(string(cfg), "credentials") || !strings.Contains(string(cfg), "frozen-model") || !strings.Contains(string(cfg), "deleted-profile") {
			t.Fatal("session snapshot changed", f.id, id, string(cfg))
		}
		if id != nil {
			var key string
			var active bool
			if err := pool.QueryRow(t.Context(), "SELECT credential_key,active FROM llm_accounts WHERE id=$1", *id).Scan(&key, &active); err != nil {
				t.Fatal(err)
			}
			if key != f.key || active != (f.key == "current-auth.json") {
				t.Fatal(key, active)
			}
		}
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM account_limit_observations o JOIN llm_accounts a ON a.id::text=o.account_id WHERE o.source_fingerprint=$1 AND o.last_attempt_at='2026-11-01T00:00:00Z'`, fingerprint).Scan(&count); err != nil || count != 1 {
		t.Fatal("observation changed", count, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORPHEUS_MIGRATION_CONFIG_FILE", "")
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatal("replay still reads legacy file", err)
	}
}
func TestLLMImportAPIKeyAndOwnerBinding(t *testing.T) {
	pool := testutil.DatabaseAt(t, 29)
	migrationInput(t, "[profiles.key]\nharness='codex'\nmodel='model'\n[profiles.key.auth]\nmode='api_key'\napi_key_env='LEGACY_LLM_TEST_KEY'\n")
	t.Setenv("LEGACY_LLM_TEST_KEY", "not-a-real-key")
	key := base64.URLEncoding.EncodeToString(make([]byte, 32))
	t.Setenv("ENV_ENCRYPTION_KEY", key)
	db := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer func() { _ = db.Close() }()
	p, err := migrate.Provider(db, "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	var id uuid.UUID
	var value secret.Value
	if err := pool.QueryRow(t.Context(), "SELECT a.id,s.nonce,s.ciphertext FROM llm_accounts a JOIN secrets s ON s.id=a.api_key_secret_id").Scan(&id, &value.Nonce, &value.Ciphertext); err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.New(key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := cipher.DecryptAccount(id, &value)
	if err != nil || got != "not-a-real-key" {
		t.Fatal("key import failed", err)
	}
	if _, err := cipher.DecryptAccount(uuid.New(), &value); err == nil {
		t.Fatal("key not bound to account")
	}
}
func TestLLMImportFailureRollsBackDataAndVersion(t *testing.T) {
	pool := testutil.DatabaseAt(t, 29)
	migrationInput(t, llmTOML)
	if _, err := pool.Exec(t.Context(), "INSERT INTO sessions(id,configuration) VALUES($1,'{}')", uuid.New()); err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer func() { _ = db.Close() }()
	p, err := migrate.Provider(db, "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Up(t.Context()); err == nil {
		t.Fatal("missing required source accepted")
	}
	var count int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM llm_accounts").Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	current, _, err := p.GetVersions(t.Context())
	if err != nil || current != 30 {
		t.Fatal(current, err)
	}
}

func TestLLMSchemaUpDown(t *testing.T) {
	pool := testutil.DatabaseAt(t, 29)
	database := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer func() { _ = database.Close() }()
	p, err := migrate.Provider(database, "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := p.UpTo(t.Context(), 30); err != nil {
			t.Fatal(err)
		}
		var table string
		if err := pool.QueryRow(t.Context(), "SELECT 'llm_accounts'::regclass::text").Scan(&table); err != nil {
			t.Fatal(err)
		}
		if _, err := p.DownTo(t.Context(), 29); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='sessions' AND column_name='llm_account_id'").Scan(&count); err != nil || count != 0 {
			t.Fatal(count, err)
		}
	}
}
func TestLLMClosedHistoryRequiresCompletedCleanup(t *testing.T) {
	for _, pending := range []string{"reserved", "active run", "pending hook", "pending operation", "unadopted sandbox"} {
		t.Run(pending, func(t *testing.T) {
			pool := testutil.DatabaseAt(t, 29)
			migrationInput(t, llmTOML)
			sid, rid := uuid.New(), uuid.New()
			if _, err := pool.Exec(t.Context(), "INSERT INTO sessions(id,configuration,sandbox_state,slot_reserved) VALUES($1,'{}','deleted',false)", sid); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), "INSERT INTO runs(id,session_id,number,status) VALUES($1,$2,1,'completed')", rid, sid); err != nil {
				t.Fatal(err)
			}
			var err error
			switch pending {
			case "reserved":
				_, err = pool.Exec(t.Context(), "UPDATE sessions SET slot_reserved=true WHERE id=$1", sid)
			case "active run":
				_, err = pool.Exec(t.Context(), "UPDATE runs SET status='accepted' WHERE id=$1", rid)
			case "pending hook":
				_, err = pool.Exec(t.Context(), "INSERT INTO hook_executions(id,session_id,run_id,name,status) VALUES($1,$2,$3,'after_run','pending')", uuid.New(), sid, rid)
			case "pending operation":
				_, err = pool.Exec(t.Context(), "INSERT INTO operations(id,session_id,kind,status) VALUES($1,$2,'sandbox_delete','uncertain')", uuid.New(), sid)
			case "unadopted sandbox":
				_, err = pool.Exec(t.Context(), `INSERT INTO operations(id,session_id,kind,status,result) VALUES($1,$2,'sandbox_create','confirmed','{"sandbox_id":"orphan"}')`, uuid.New(), sid)
			}
			if err != nil {
				t.Fatal(err)
			}
			database := stdlib.OpenDB(*pool.Config().ConnConfig)
			defer func() { _ = database.Close() }()
			p, err := migrate.Provider(database, "../../migrations")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Up(t.Context()); err == nil || !strings.Contains(err.Error(), "invalid_legacy_credentials") {
				t.Fatal("unfinished cleanup accepted", err)
			}
		})
	}
}
func TestLLMObservationCollisionRollsBackSessionAndCatalog(t *testing.T) {
	pool := testutil.DatabaseAt(t, 29)
	migrationInput(t, llmTOML)
	sid := uuid.New()
	raw := `{"harness":"codex","credentials":{"mode":"account","account_id":"renamed","store":{"type":"s3","region":"us-east-1","bucket":"b"},"key":"current-auth.json"}}`
	if _, err := pool.Exec(t.Context(), "INSERT INTO sessions(id,configuration) VALUES($1,$2)", sid, raw); err != nil {
		t.Fatal(err)
	}
	fingerprint, err := config.SourceFingerprint("codex", session.Credentials{Mode: "account", Store: &session.CredentialStore{Type: "s3", Region: "us-east-1", Bucket: "b"}, Key: "current-auth.json"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"current-account", "renamed"} {
		if _, err := pool.Exec(t.Context(), "INSERT INTO account_limit_observations(account_id,source_fingerprint,last_attempt_at) VALUES($1,$2,now())", name, fingerprint); err != nil {
			t.Fatal(err)
		}
	}
	database := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer func() { _ = database.Close() }()
	p, err := migrate.Provider(database, "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Up(t.Context()); err == nil || !strings.Contains(err.Error(), "observation mapping conflicts") {
		t.Fatal(err)
	}
	var count int
	var unchanged bool
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM llm_accounts").Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT llm_account_id IS NULL AND configuration ? 'credentials' FROM sessions WHERE id=$1", sid).Scan(&unchanged); err != nil || !unchanged {
		t.Fatal(unchanged, err)
	}
}
func TestLLMMissingImportInputRefusesExistingData(t *testing.T) {
	t.Setenv("ORPHEUS_MIGRATION_CONFIG_FILE", "")
	pool := testutil.DatabaseAt(t, 29)
	if _, err := pool.Exec(t.Context(), "INSERT INTO sessions(id,configuration) VALUES($1,'{}')", uuid.New()); err != nil {
		t.Fatal(err)
	}
	database := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer func() { _ = database.Close() }()
	p, err := migrate.Provider(database, "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Up(t.Context()); err == nil || !strings.Contains(err.Error(), "ORPHEUS_MIGRATION_CONFIG_FILE") {
		t.Fatal(err)
	}
}
