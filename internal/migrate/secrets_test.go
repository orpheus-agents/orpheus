//go:build integration

package migrate_test

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/orpheus-agents/orpheus/internal/migrate"
	"github.com/orpheus-agents/orpheus/internal/secret"
	"github.com/orpheus-agents/orpheus/internal/testutil"
	"github.com/pressly/goose/v3"
)

func secretsMigration(t *testing.T) (*pgxpool.Pool, *goose.Provider) {
	t.Helper()
	pool := testutil.Database(t)
	database := stdlib.OpenDB(*pool.Config().ConnConfig)
	t.Cleanup(func() { _ = database.Close() })
	p, err := migrate.Provider(database, "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(t.Context(), 24); err != nil {
		t.Fatal(err)
	}
	return pool, p
}

// Reproduce the pre-migration writer independently of the new Value codec.
func legacyEnvironment(t *testing.T, sid, rid uuid.UUID, env map[string]string) string {
	t.Helper()
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	prefix, aad := "aes-gcm-v1:", sid[:]
	if rid != uuid.Nil {
		prefix = "aes-gcm-run-v1:"
		aad = append([]byte("orpheus-run-env-v1\x00"), sid[:]...)
		aad = append(aad, rid[:]...)
	}
	nonce := bytes.Repeat([]byte{17}, 12)
	return prefix + base64.RawURLEncoding.EncodeToString(aead.Seal(nonce, nonce, data, aad))
}

func TestSecretsMigrationRoundTripAndCatchup(t *testing.T) {
	pool, p := secretsMigration(t)
	sid, rid := uuid.New(), uuid.New()
	sessionEnv := map[string]string{"TOKEN": strings.Repeat("value", 100), "EMPTY": "", "UNICODE": "секрет"}
	runEnv := map[string]string{"TOKEN": "run-value!"}
	sessionToken := legacyEnvironment(t, sid, uuid.Nil, sessionEnv)
	runToken := legacyEnvironment(t, sid, rid, runEnv)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := pool.Exec(t.Context(), `INSERT INTO sessions(id,configuration,env_ciphertext,created_at) VALUES($1,'{}',$2,$3)`, sid, sessionToken, at); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO runs(id,session_id,number,env_ciphertext,created_at) VALUES($1,$2,1,$3,$4)`, rid, sid, runToken, at); err != nil {
		t.Fatal(err)
	}
	empty := uuid.New()
	if _, err := pool.Exec(t.Context(), `INSERT INTO sessions(id,configuration) VALUES($1,'{}')`, empty); err != nil {
		t.Fatal(err)
	}
	c, err := secret.New(base64.URLEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := p.UpTo(t.Context(), 26); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"sessions", "runs"} {
			id, want, original := sid, sessionEnv, sessionToken
			if table == "runs" {
				id, want, original = rid, runEnv, runToken
			}
			var v secret.Value
			var token string
			var created time.Time
			if err := pool.QueryRow(t.Context(), "SELECT x.nonce,x.ciphertext,x.created_at,o.env_ciphertext FROM "+table+" o JOIN secrets x ON x.id=o.env_secret_id WHERE o.id=$1", id).Scan(&v.Nonce, &v.Ciphertext, &created, &token); err != nil {
				t.Fatal(err)
			}
			if !created.Equal(at) || token != original {
				t.Fatal("source or timestamp changed")
			}
			var got map[string]string
			if table == "sessions" {
				got, err = c.Decrypt(sid, &v)
				if legacyToken(&v, false) != original {
					t.Fatal("session bytes changed")
				}
			} else {
				got, err = c.DecryptRun(sid, rid, &v)
				if legacyToken(&v, true) != original {
					t.Fatal("run bytes changed")
				}
			}
			if err != nil || !maps.Equal(got, want) {
				t.Fatal("migrated environment differs", err)
			}
		}
		var count int
		var ref *uuid.UUID
		if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM secrets").Scan(&count); err != nil || count != 2 {
			t.Fatal(count, err)
		}
		if err := pool.QueryRow(t.Context(), "SELECT env_secret_id FROM sessions WHERE id=$1", empty).Scan(&ref); err != nil || ref != nil {
			t.Fatal("empty environment migrated", err)
		}
		// Re-execute the data migration with references intact; it must not duplicate.
		if _, err := p.DownTo(t.Context(), 25); err != nil {
			t.Fatal(err)
		}
		if _, err := p.UpTo(t.Context(), 26); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM secrets").Scan(&count); err != nil || count != 2 {
			t.Fatal("repeat duplicated secrets", count, err)
		}
		if _, err := p.DownTo(t.Context(), 24); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			table string
			id    uuid.UUID
			token string
		}{{"sessions", sid, sessionToken}, {"runs", rid, runToken}} {
			var token string
			if err := pool.QueryRow(t.Context(), "SELECT env_ciphertext FROM "+tc.table+" WHERE id=$1", tc.id).Scan(&token); err != nil || token != tc.token {
				t.Fatal("rollback lost environment", err)
			}
		}
	}
	// A writer from the preceding release may insert after the first backfill.
	if _, err := p.UpTo(t.Context(), 26); err != nil {
		t.Fatal(err)
	}
	late := uuid.New()
	lateEnv := map[string]string{"TOKEN": "run-value"}
	token := legacyEnvironment(t, late, uuid.Nil, lateEnv)
	if (len(sessionToken)-11)%4 != 0 || (len(runToken)-15)%4 != 3 || (len(token)-11)%4 != 2 {
		t.Fatal("fixtures must cover all three base64 padding lengths")
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO sessions(id,configuration,env_ciphertext) VALUES($1,'{}',$2)`, late, token); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(t.Context(), 25); err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(t.Context(), 26); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM secrets").Scan(&count); err != nil || count != 3 {
		t.Fatal("catchup failed", count, err)
	}
	var value secret.Value
	if err := pool.QueryRow(t.Context(), "SELECT x.nonce,x.ciphertext FROM sessions s JOIN secrets x ON x.id=s.env_secret_id WHERE s.id=$1", late).Scan(&value.Nonce, &value.Ciphertext); err != nil {
		t.Fatal(err)
	}
	got, err := c.Decrypt(late, &value)
	if err != nil || !maps.Equal(got, lateEnv) || legacyToken(&value, false) != token {
		t.Fatal("catchup changed payload", err)
	}
}

func TestSecretsMigrationRejectsInvalidFormatAtomically(t *testing.T) {
	for _, table := range []string{"sessions", "runs"} {
		for _, bad := range []string{"fernet-v1:" + strings.Repeat("A", 40), "aes-gcm-v1:" + strings.Repeat("A", 40), "aes-gcm-run-v1:" + strings.Repeat("A", 40), "", strings.Repeat("A", 37), strings.Repeat("A", 41), strings.Repeat("A", 38) + "=", strings.Repeat("A", 38) + "!", strings.Repeat("A", 38) + "\n"} {
			t.Run(table+"/"+base64.RawURLEncoding.EncodeToString([]byte(bad)), func(t *testing.T) {
				pool, p := secretsMigration(t)
				sid, rid := uuid.New(), uuid.New()
				st, rt := legacyEnvironment(t, sid, uuid.Nil, map[string]string{"TOKEN": "ok"}), legacyEnvironment(t, sid, rid, map[string]string{"TOKEN": "ok"})
				prefix := "aes-gcm-v1:"
				if table == "runs" {
					prefix = "aes-gcm-run-v1:"
				}
				invalid := prefix + bad
				if strings.Contains(bad, ":") {
					if strings.HasPrefix(bad, prefix) {
						t.Skip("valid prefix for this table")
					}
					invalid = bad
				}
				if table == "sessions" {
					st = invalid
				} else {
					rt = invalid
				}
				if _, err := pool.Exec(t.Context(), `INSERT INTO sessions(id,configuration,env_ciphertext) VALUES($1,'{}',$2)`, sid, st); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(t.Context(), `INSERT INTO runs(id,session_id,number,env_ciphertext) VALUES($1,$2,1,$3)`, rid, sid, rt); err != nil {
					t.Fatal(err)
				}
				if _, err := p.UpTo(t.Context(), 26); err == nil {
					t.Fatal("accepted invalid source")
				}
				var count int
				if err := pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM secrets)+(SELECT count(*) FROM sessions WHERE env_secret_id IS NOT NULL)+(SELECT count(*) FROM runs WHERE env_secret_id IS NOT NULL)`).Scan(&count); err != nil || count != 0 {
					t.Fatal("partial data migration", count, err)
				}
			})
		}
	}
}

func TestSecretsSchemaConstraints(t *testing.T) {
	pool := testutil.Database(t)
	for _, sizes := range [][2]int{{11, 16}, {13, 16}, {12, 15}} {
		if _, err := pool.Exec(t.Context(), "INSERT INTO secrets(id,nonce,ciphertext) VALUES($1,$2,$3)", uuid.New(), make([]byte, sizes[0]), make([]byte, sizes[1])); err == nil {
			t.Fatal("invalid lengths accepted", sizes)
		}
	}
}

// Test-only encoder for the legacy wire format; application code has no codec.
func legacyToken(value *secret.Value, run bool) string {
	prefix := "aes-gcm-v1:"
	if run {
		prefix = "aes-gcm-run-v1:"
	}
	return prefix + base64.RawURLEncoding.EncodeToString(append(bytes.Clone(value.Nonce), value.Ciphertext...))
}
