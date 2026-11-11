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
	"github.com/orpheus-agents/orpheus/internal/secret"
)

func TestSecretsCatchupPreservesExistingAndLateWrites(t *testing.T) {
	pool, p := secretsMigration(t)
	old := uuid.New()
	oldToken := legacyEnvironment(t, old, uuid.Nil, map[string]string{"TOKEN": "old"})
	if _, err := pool.Exec(t.Context(), `INSERT INTO sessions(id,configuration,env_ciphertext) VALUES($1,'{}',$2)`, old, oldToken); err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(t.Context(), 26); err != nil {
		t.Fatal(err)
	}
	var oldRef uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT env_secret_id FROM sessions WHERE id=$1`, old).Scan(&oldRef); err != nil {
		t.Fatal(err)
	}
	type owner struct {
		table   string
		id, sid uuid.UUID
		token   string
		env     map[string]string
	}
	var owners []owner
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	// Consecutive JSON lengths exercise all three base64url padding lengths.
	for n := range 3 {
		sid, rid := uuid.New(), uuid.New()
		env := map[string]string{"TOKEN": strings.Repeat("x", 130+n), "UNICODE": "секрет", "EMPTY": ""}
		st, rt := legacyEnvironment(t, sid, uuid.Nil, env), legacyEnvironment(t, sid, rid, env)
		if _, err := pool.Exec(t.Context(), `INSERT INTO sessions(id,configuration,env_ciphertext,created_at) VALUES($1,'{}',$2,$3)`, sid, st, at); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO runs(id,session_id,number,env_ciphertext,created_at) VALUES($1,$2,1,$3,$4)`, rid, sid, rt, at); err != nil {
			t.Fatal(err)
		}
		owners = append(owners, owner{"sessions", sid, sid, st, env}, owner{"runs", rid, sid, rt, env})
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
		if _, err := p.Up(t.Context()); err != nil {
			t.Fatal(err)
		}
		for _, o := range owners {
			var value secret.Value
			var token string
			var created time.Time
			if err := pool.QueryRow(t.Context(), "SELECT x.nonce,x.ciphertext,x.created_at,o.env_ciphertext FROM "+o.table+" o JOIN secrets x ON x.id=o.env_secret_id WHERE o.id=$1", o.id).Scan(&value.Nonce, &value.Ciphertext, &created, &token); err != nil {
				t.Fatal(err)
			}
			var got map[string]string
			if o.table == "sessions" {
				got, err = c.Decrypt(o.id, &value)
			} else {
				got, err = c.DecryptRun(o.sid, o.id, &value)
			}
			if err != nil || !maps.Equal(got, o.env) || token != o.token || !created.Equal(at) || legacyToken(&value, o.table == "runs") != o.token {
				t.Fatal("catchup changed environment", err)
			}
		}
		var ref uuid.UUID
		var count int
		var emptyRef *uuid.UUID
		if err := pool.QueryRow(t.Context(), `SELECT env_secret_id FROM sessions WHERE id=$1`, old).Scan(&ref); err != nil || ref != oldRef {
			t.Fatal("existing reference changed", err)
		}
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM secrets`).Scan(&count); err != nil || count != 7 {
			t.Fatal("secrets duplicated", count, err)
		}
		if err := pool.QueryRow(t.Context(), `SELECT env_secret_id FROM sessions WHERE id=$1`, empty).Scan(&emptyRef); err != nil || emptyRef != nil {
			t.Fatal("empty environment copied", err)
		}
		if _, err := p.DownTo(t.Context(), 26); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSecretsCatchupDownPreservesTableOnlyWritesThroughSchemaRollback(t *testing.T) {
	pool, p := secretsMigration(t)
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	c, err := secret.New(base64.URLEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	sid, rid := uuid.New(), uuid.New()
	env := map[string]string{"TOKEN": strings.Repeat("secret", 80), "EMPTY": "", "UNICODE": "секрет"}
	sv, err := c.Encrypt(sid, env)
	if err != nil {
		t.Fatal(err)
	}
	rv, err := c.EncryptRun(sid, rid, env)
	if err != nil {
		t.Fatal(err)
	}
	sref, rref := uuid.New(), uuid.New()
	for _, x := range []struct {
		id uuid.UUID
		v  *secret.Value
	}{{sref, sv}, {rref, rv}} {
		if _, err := pool.Exec(t.Context(), `INSERT INTO secrets(id,nonce,ciphertext) VALUES($1,$2,$3)`, x.id, x.v.Nonce, x.v.Ciphertext); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO sessions(id,configuration,env_secret_id) VALUES($1,'{}',$2)`, sid, sref); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO runs(id,session_id,number,env_secret_id) VALUES($1,$2,1,$3)`, rid, sid, rref); err != nil {
		t.Fatal(err)
	}
	for _, target := range []int64{26, 24} {
		if _, err := p.DownTo(t.Context(), target); err != nil {
			t.Fatal(err)
		}
		for _, o := range []struct {
			table string
			id    uuid.UUID
			v     *secret.Value
		}{{"sessions", sid, sv}, {"runs", rid, rv}} {
			var token string
			if err := pool.QueryRow(t.Context(), "SELECT env_ciphertext FROM "+o.table+" WHERE id=$1", o.id).Scan(&token); err != nil {
				t.Fatal(err)
			}
			if token != legacyToken(o.v, o.table == "runs") {
				t.Fatal("rollback changed encrypted bytes")
			}
			// This reader reproduces the pre-table AES-GCM format independently.
			if got := decryptLegacyEnvironment(t, sid, o.id, o.table == "runs", token); !maps.Equal(got, env) {
				t.Fatal("old reader lost environment")
			}
		}
	}
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM secrets`).Scan(&count); err != nil || count != 2 {
		t.Fatal("upgrade after full rollback failed", count, err)
	}
}

func decryptLegacyEnvironment(t *testing.T, sid, rid uuid.UUID, run bool, token string) map[string]string {
	t.Helper()
	prefix, aad := "aes-gcm-v1:", bytes.Clone(sid[:])
	if run {
		prefix = "aes-gcm-run-v1:"
		aad = append(append([]byte("orpheus-run-env-v1\x00"), sid[:]...), rid[:]...)
	}
	payload, ok := strings.CutPrefix(token, prefix)
	if !ok {
		t.Fatal("wrong legacy prefix")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || len(raw) < 28 {
		t.Fatal("invalid legacy payload", err)
	}
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := aead.Open(nil, raw[:12], raw[12:], aad)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]string
	if err := json.Unmarshal(plain, &values); err != nil {
		t.Fatal(err)
	}
	return values
}

func TestSecretsCatchupRejectsInvalidOrUnmovedRowsAtomically(t *testing.T) {
	for _, mode := range []string{"invalid session", "invalid run", "unmoved session", "unmoved run"} {
		t.Run(mode, func(t *testing.T) {
			pool, p := secretsMigration(t)
			if _, err := p.UpTo(t.Context(), 26); err != nil {
				t.Fatal(err)
			}
			sid, rid := uuid.New(), uuid.New()
			st, rt := legacyEnvironment(t, sid, uuid.Nil, map[string]string{"TOKEN": "ok"}), legacyEnvironment(t, sid, rid, map[string]string{"TOKEN": "ok"})
			if mode == "invalid session" {
				st = "invalid"
			}
			if mode == "invalid run" {
				rt = "invalid"
			}
			if _, err := pool.Exec(t.Context(), `INSERT INTO sessions(id,configuration,env_ciphertext) VALUES($1,'{}',$2)`, sid, st); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `INSERT INTO runs(id,session_id,number,env_ciphertext) VALUES($1,$2,1,$3)`, rid, sid, rt); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(mode, "unmoved") {
				table := "sessions"
				if mode == "unmoved run" {
					table = "runs"
				}
				if _, err := pool.Exec(t.Context(), `CREATE FUNCTION skip_secret_catchup() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$; CREATE TRIGGER skip_secret_catchup BEFORE UPDATE ON `+table+` FOR EACH ROW EXECUTE FUNCTION skip_secret_catchup()`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := p.Up(t.Context()); err == nil {
				t.Fatal("accepted remaining legacy-only rows")
			}
			var count int
			if err := pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM secrets)+(SELECT count(*) FROM sessions WHERE env_secret_id IS NOT NULL)+(SELECT count(*) FROM runs WHERE env_secret_id IS NOT NULL)`).Scan(&count); err != nil || count != 0 {
				t.Fatal("partial migration", count, err)
			}
			version, err := p.GetDBVersion(t.Context())
			if err != nil || version != 26 {
				t.Fatal("failed migration advanced version", version, err)
			}
		})
	}
}
