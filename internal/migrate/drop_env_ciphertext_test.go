//go:build integration

package migrate_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/orpheus-agents/orpheus/internal/secret"
	"github.com/pressly/goose/v3"
)

func legacyColumns(t *testing.T, pool *pgxpool.Pool, want int) {
	t.Helper()
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name IN ('sessions','runs') AND column_name='env_ciphertext'`).Scan(&count); err != nil || count != want {
		t.Fatal("unexpected legacy columns", count, err)
	}
}

func beforeSecretCleanup(t *testing.T) (*pgxpool.Pool, *goose.Provider) {
	t.Helper()
	pool, p := secretsMigration(t)
	if _, err := p.UpTo(t.Context(), 27); err != nil {
		t.Fatal(err)
	}
	return pool, p
}

func TestDropEnvironmentRoundTrip(t *testing.T) {
	pool, p := beforeSecretCleanup(t)
	c, err := secret.New(base64.URLEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	type owner struct {
		table        string
		id, sid, ref uuid.UUID
		value        *secret.Value
		env          map[string]string
	}
	var owners []owner
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	// Cover each base64 padding length, wrapped PostgreSQL base64 output,
	// legacy copies and table-only writes from RFC-033, for both scopes.
	for _, dual := range []bool{true, false} {
		for n := range 3 {
			sid, rid := uuid.New(), uuid.New()
			env := map[string]string{"TOKEN": strings.Repeat("x", 130+n), "UNICODE": "секрет", "EMPTY": ""}
			sv, err := c.Encrypt(sid, env)
			if err != nil {
				t.Fatal(err)
			}
			rv, err := c.EncryptRun(sid, rid, env)
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range []owner{{"sessions", sid, sid, uuid.New(), sv, env}, {"runs", rid, sid, uuid.New(), rv, env}} {
				if _, err := pool.Exec(t.Context(), `INSERT INTO secrets(id,nonce,ciphertext,created_at) VALUES($1,$2,$3,$4)`, o.ref, o.value.Nonce, o.value.Ciphertext, at); err != nil {
					t.Fatal(err)
				}
				var token *string
				if dual {
					token = new(legacyToken(o.value, o.table == "runs"))
				}
				if o.table == "sessions" {
					_, err = pool.Exec(t.Context(), `INSERT INTO sessions(id,configuration,env_secret_id,env_ciphertext) VALUES($1,'{}',$2,$3)`, o.id, o.ref, token)
				} else {
					_, err = pool.Exec(t.Context(), `INSERT INTO runs(id,session_id,number,env_secret_id,env_ciphertext) VALUES($1,$2,1,$3,$4)`, o.id, o.sid, o.ref, token)
				}
				if err != nil {
					t.Fatal(err)
				}
				owners = append(owners, o)
			}
		}
	}
	emptySID, emptyRID := uuid.New(), uuid.New()
	if _, err := pool.Exec(t.Context(), `INSERT INTO sessions(id,configuration) VALUES($1,'{}')`, emptySID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO runs(id,session_id,number) VALUES($1,$2,1)`, emptyRID, emptySID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := p.UpTo(t.Context(), 28); err != nil {
			t.Fatal(err)
		}
		legacyColumns(t, pool, 0)
		var count int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM secrets`).Scan(&count); err != nil || count != len(owners) {
			t.Fatal("secret count changed", count, err)
		}
		for _, o := range owners {
			var ref uuid.UUID
			var value secret.Value
			var created time.Time
			if err := pool.QueryRow(t.Context(), "SELECT o.env_secret_id,x.nonce,x.ciphertext,x.created_at FROM "+o.table+" o JOIN secrets x ON x.id=o.env_secret_id WHERE o.id=$1", o.id).Scan(&ref, &value.Nonce, &value.Ciphertext, &created); err != nil {
				t.Fatal(err)
			}
			if ref != o.ref || !bytes.Equal(value.Nonce, o.value.Nonce) || !bytes.Equal(value.Ciphertext, o.value.Ciphertext) || !created.Equal(at) {
				t.Fatal("cleanup changed secret")
			}
			var got map[string]string
			if o.table == "sessions" {
				got, err = c.Decrypt(o.id, &value)
			} else {
				got, err = c.DecryptRun(o.sid, o.id, &value)
			}
			if err != nil || !maps.Equal(got, o.env) {
				t.Fatal("cleanup lost environment", err)
			}
		}
		if _, err := p.DownTo(t.Context(), 27); err != nil {
			t.Fatal(err)
		}
		legacyColumns(t, pool, 2)
		for _, o := range owners {
			var token string
			if err := pool.QueryRow(t.Context(), "SELECT env_ciphertext FROM "+o.table+" WHERE id=$1", o.id).Scan(&token); err != nil {
				t.Fatal(err)
			}
			if token != legacyToken(o.value, o.table == "runs") {
				t.Fatal("rollback changed encrypted bytes")
			}
			if got := decryptLegacyEnvironment(t, o.sid, o.id, o.table == "runs", token); !maps.Equal(got, o.env) {
				t.Fatal("rollback lost environment")
			}
		}
		var nonempty int
		if err := pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM sessions WHERE id=$1 AND (env_secret_id IS NOT NULL OR env_ciphertext IS NOT NULL))+(SELECT count(*) FROM runs WHERE id=$2 AND (env_secret_id IS NOT NULL OR env_ciphertext IS NOT NULL))`, emptySID, emptyRID).Scan(&nonempty); err != nil || nonempty != 0 {
			t.Fatal("rollback invented environment", err)
		}
	}
}

func TestDropEnvironmentRejectsUnsafeData(t *testing.T) {
	for _, table := range []string{"sessions", "runs"} {
		for _, mode := range []string{"legacy only", "mismatch"} {
			t.Run(table+"/"+mode, func(t *testing.T) {
				pool, p := beforeSecretCleanup(t)
				sid, rid, ref := uuid.New(), uuid.New(), uuid.New()
				if _, err := pool.Exec(t.Context(), `INSERT INTO sessions(id,configuration) VALUES($1,'{}')`, sid); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(t.Context(), `INSERT INTO runs(id,session_id,number) VALUES($1,$2,1)`, rid, sid); err != nil {
					t.Fatal(err)
				}
				id := sid
				token := legacyEnvironment(t, sid, uuid.Nil, map[string]string{"TOKEN": "private"})
				if table == "runs" {
					id = rid
					token = legacyEnvironment(t, sid, rid, map[string]string{"TOKEN": "private"})
				}
				var link *uuid.UUID
				want := "env_ciphertext without secret"
				if mode == "mismatch" {
					link = &ref
					want = "env_ciphertext differs from its secret"
					if _, err := pool.Exec(t.Context(), `INSERT INTO secrets(id,nonce,ciphertext) VALUES($1,$2,$3)`, ref, make([]byte, 12), make([]byte, 16)); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := pool.Exec(t.Context(), "UPDATE "+table+" SET env_ciphertext=$2,env_secret_id=$3 WHERE id=$1", id, token, link); err != nil {
					t.Fatal(err)
				}
				if _, err := p.UpTo(t.Context(), 28); err == nil || !strings.Contains(err.Error(), want) {
					t.Fatal("unsafe migration accepted", err)
				}
				legacyColumns(t, pool, 2)
				version, err := p.GetDBVersion(t.Context())
				if err != nil || version != 27 {
					t.Fatal("failed migration advanced version", version, err)
				}
				var got string
				var gotRef *uuid.UUID
				if err := pool.QueryRow(t.Context(), "SELECT env_ciphertext,env_secret_id FROM "+table+" WHERE id=$1", id).Scan(&got, &gotRef); err != nil || got != token || (gotRef == nil) != (link == nil) || (gotRef != nil && *gotRef != ref) {
					t.Fatal("failed migration changed source", err)
				}
			})
		}
	}
}

func TestDropEnvironmentLockTimeoutAndRetry(t *testing.T) {
	for _, table := range []string{"sessions", "runs"} {
		t.Run(table, func(t *testing.T) {
			pool, p := beforeSecretCleanup(t)
			tx, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			if _, err := tx.Exec(t.Context(), "LOCK TABLE "+table+" IN ACCESS SHARE MODE"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			start := time.Now()
			_, err = p.UpTo(ctx, 28)
			elapsed := time.Since(start)
			pgerr, ok := errors.AsType[*pgconn.PgError](err)
			if !ok || pgerr.Code != "55P03" || elapsed < 4500*time.Millisecond || elapsed > 10*time.Second {
				t.Fatal("expected five-second lock timeout", elapsed, err)
			}
			// A lock on runs must roll back the already completed sessions DROP too.
			legacyColumns(t, pool, 2)
			version, err := p.GetDBVersion(t.Context())
			if err != nil || version != 27 {
				t.Fatal("failed migration advanced version", version, err)
			}
			if err := tx.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := p.UpTo(t.Context(), 28); err != nil {
				t.Fatal("retry failed", err)
			}
			legacyColumns(t, pool, 0)
		})
	}
}
