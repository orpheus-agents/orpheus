//go:build integration

package store

import (
	"bytes"
	"maps"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/secret"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

func secretCount(t *testing.T, s *Store, want int) {
	t.Helper()
	var count int
	if err := s.Pool.QueryRow(t.Context(), "SELECT count(*) FROM secrets").Scan(&count); err != nil || count != want {
		t.Fatalf("secrets: got %d, want %d: %v", count, want, err)
	}
	var orphans int
	if err := s.Pool.QueryRow(t.Context(), `SELECT count(*) FROM secrets x WHERE NOT EXISTS(SELECT 1 FROM sessions s WHERE s.env_secret_id=x.id) AND NOT EXISTS(SELECT 1 FROM runs r WHERE r.env_secret_id=x.id)`).Scan(&orphans); err != nil || orphans != 0 {
		t.Fatal("orphan secrets", orphans, err)
	}
}

func TestSecretDualWriteAndConcurrentReplay(t *testing.T) {
	s := fixture(t)
	req := request()
	req.Create.Env = map[string]string{"RUN_TOKEN": "run-private"}
	results := make(chan session.Acceptance, 8)
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { a, err := s.Accept(t.Context(), req); results <- a; errs <- err })
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first session.Acceptance
	for a := range results {
		if first.SessionID == uuid.Nil {
			first = a
		}
		if a != first {
			t.Fatal("idempotency split")
		}
	}
	secretCount(t, s, 2)
	record := mustSession(t, s, first.SessionID)
	run, err := GetRun(t.Context(), s.Pool, first.SessionID, first.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id    *uuid.UUID
		token *string
		parse func(*string) (*secret.Value, error)
	}{{record.EnvSecretID, record.EnvCiphertext, secret.ParseSession}, {run.EnvSecretID, run.EnvCiphertext, secret.ParseRun}} {
		if tc.id == nil || tc.token == nil {
			t.Fatal("missing dual write")
		}
		row, err := db.New(s.Pool).GetSecret(t.Context(), *tc.id)
		if err != nil {
			t.Fatal(err)
		}
		value, err := tc.parse(tc.token)
		if err != nil || !bytes.Equal(row.Nonce, value.Nonce) || !bytes.Equal(row.Ciphertext, value.Ciphertext) {
			t.Fatal("dual writes differ", err)
		}
		if _, err := s.Pool.Exec(t.Context(), "DELETE FROM secrets WHERE id=$1", *tc.id); err == nil {
			t.Fatal("deleted a referenced secret")
		}
	}
	if record.EnvSecretID == nil || run.EnvSecretID == nil || *record.EnvSecretID == *run.EnvSecretID {
		t.Fatal("owners share a secret")
	}
	req.Create.Env["RUN_TOKEN"] = "changed"
	_, err = s.Accept(t.Context(), req)
	requireCode(t, err, "idempotency_conflict")
	secretCount(t, s, 2)
}

func TestSecretEmptyAndTransactionRollback(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		s := fixture(t)
		req := request()
		req.Create.Configuration.Sandbox.Env = nil
		a, err := s.Accept(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		r := mustSession(t, s, a.SessionID)
		run, err := GetRun(t.Context(), s.Pool, a.SessionID, a.RunID)
		if err != nil || r.EnvSecretID != nil || r.EnvCiphertext != nil || run.EnvSecretID != nil || run.EnvCiphertext != nil {
			t.Fatal("empty environment stored", err)
		}
		secretCount(t, s, 0)
	})
	t.Run("failed session admission", func(t *testing.T) {
		s := fixture(t)
		s.Settings.MaxConcurrentSessions = 0
		_, err := s.Accept(t.Context(), request())
		requireCode(t, err, "capacity_exhausted")
		secretCount(t, s, 0)
	})
	t.Run("failed run admission", func(t *testing.T) {
		s := fixture(t)
		a, err := s.Accept(t.Context(), request())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Cancel(t.Context(), a.SessionID, a.RunID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Pool.Exec(t.Context(), `CREATE FUNCTION reject_secret_run() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test failure'; END $$; CREATE TRIGGER reject_secret_run BEFORE INSERT ON runs FOR EACH ROW EXECUTE FUNCTION reject_secret_run()`); err != nil {
			t.Fatal(err)
		}
		_, err = s.Accept(t.Context(), Admission{SessionID: a.SessionID, Key: uuid.New(), Env: map[string]string{"TOKEN": "new"}, Messages: []session.TextMessage{{Text: "next"}}})
		if err == nil {
			t.Fatal("expected insertion failure")
		}
		secretCount(t, s, 1)
	})
}

func TestSecretReadPrecedenceAndReplay(t *testing.T) {
	for _, table := range []string{"sessions", "runs"} {
		for _, mode := range []string{"legacy", "bad column", "bad secret", "wrong owner", "wrong scope"} {
			t.Run(table+"/"+mode, func(t *testing.T) {
				s := fixture(t)
				req := request()
				req.Create.Env = map[string]string{"TOKEN": "run-private"}
				a, err := s.Accept(t.Context(), req)
				if err != nil {
					t.Fatal(err)
				}
				id := a.SessionID
				if table == "runs" {
					id = a.RunID
				}
				switch mode {
				case "legacy":
					_, err = s.Pool.Exec(t.Context(), "UPDATE "+table+" SET env_secret_id=NULL WHERE id=$1", id)
				case "bad column":
					_, err = s.Pool.Exec(t.Context(), "UPDATE "+table+" SET env_ciphertext='invalid' WHERE id=$1", id)
				case "bad secret":
					_, err = s.Pool.Exec(t.Context(), "UPDATE secrets SET ciphertext=set_byte(ciphertext,0,get_byte(ciphertext,0)#1) WHERE id=(SELECT env_secret_id FROM "+table+" WHERE id=$1)", id)
				case "wrong owner":
					other, e := s.Accept(t.Context(), reqWithNewKey(req))
					if e != nil {
						t.Fatal(e)
					}
					otherID := other.SessionID
					if table == "runs" {
						otherID = other.RunID
					}
					_, err = s.Pool.Exec(t.Context(), "UPDATE "+table+" SET env_secret_id=(SELECT env_secret_id FROM "+table+" WHERE id=$2) WHERE id=$1", id, otherID)
				case "wrong scope":
					otherTable, otherID := "runs", a.RunID
					if table == "runs" {
						otherTable, otherID = "sessions", a.SessionID
					}
					_, err = s.Pool.Exec(t.Context(), "UPDATE "+table+" SET env_secret_id=(SELECT env_secret_id FROM "+otherTable+" WHERE id=$2) WHERE id=$1", id, otherID)
				}
				if err != nil {
					t.Fatal(err)
				}
				var values map[string]string
				if table == "sessions" {
					values, err = s.SessionEnvironment(t.Context(), s.Pool, mustSession(t, s, a.SessionID))
				} else {
					run, e := GetRun(t.Context(), s.Pool, a.SessionID, a.RunID)
					if e != nil {
						t.Fatal(e)
					}
					values, err = s.RunEnvironment(t.Context(), s.Pool, run)
				}
				good := mode == "legacy" || mode == "bad column"
				if good {
					want := req.Create.Configuration.Sandbox.Env
					if table == "runs" {
						want = req.Create.Env
					}
					if err != nil || !maps.Equal(values, want) {
						t.Fatal("read changed", err)
					}
					again, err := s.Accept(t.Context(), req)
					if err != nil || again != a {
						t.Fatal("replay failed", err)
					}
				} else {
					if err == nil {
						t.Fatal("accepted damaged secret")
					}
					_, err = s.Accept(t.Context(), req)
					requireCode(t, err, "storage_unavailable")
				}
			})
		}
	}
}

func reqWithNewKey(req Admission) Admission { req.Key = uuid.New(); return req }

func TestMissingReferencedSecretDoesNotUseColumn(t *testing.T) {
	s := fixture(t)
	req := request()
	req.Create.Env = map[string]string{"TOKEN": "run"}
	a, err := s.Accept(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	record := mustSession(t, s, a.SessionID)
	record.EnvSecretID = new(uuid.New())
	if _, err := s.SessionEnvironment(t.Context(), s.Pool, record); err == nil {
		t.Fatal("missing session secret used legacy column")
	}
	run, err := GetRun(t.Context(), s.Pool, a.SessionID, a.RunID)
	if err != nil {
		t.Fatal(err)
	}
	run.EnvSecretID = new(uuid.New())
	if _, err := s.RunEnvironment(t.Context(), s.Pool, run); err == nil {
		t.Fatal("missing run secret used legacy column")
	}
}
