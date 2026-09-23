//go:build integration

package migrate_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/skillum-ai/orpheus/internal/migrate"
	"github.com/skillum-ai/orpheus/internal/testutil"
)

func TestRunEnvironmentMigrations(t *testing.T) {
	pool := testutil.Database(t)
	db := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer func() { _ = db.Close() }()
	p, err := migrate.Provider(db, "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(t.Context(), 5); err != nil {
		t.Fatal(err)
	}
	sid, rid := uuid.New(), uuid.New()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := pool.Exec(t.Context(), `INSERT INTO sessions(id,configuration,next_event_sequence) VALUES($1,'{}',2)`, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO runs(id,session_id,number) VALUES($1,$2,1)`, rid, sid); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"id":"` + rid.String() + `","status":"accepted"}`)
	if _, err := pool.Exec(t.Context(), `INSERT INTO session_events(session_id,sequence,type,created_at,data) VALUES($1,1,'run.updated',$2,$3)`, sid, at, original); err != nil {
		t.Fatal(err)
	}
	check := func(up bool) {
		t.Helper()
		var raw []byte
		var created time.Time
		var sequence int64
		if err := pool.QueryRow(t.Context(), `SELECT data,created_at,sequence FROM session_events WHERE session_id=$1`, sid).Scan(&raw, &created, &sequence); err != nil {
			t.Fatal(err)
		}
		if !created.Equal(at) || sequence != 1 {
			t.Fatal("event position changed", created, sequence)
		}
		var data map[string]json.RawMessage
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatal(err)
		}
		_, hasNames := data["env_names"]
		_, hasFrom := data["env_from"]
		if hasNames != up || hasFrom != up {
			t.Fatal(data)
		}
		if up {
			if string(data["env_names"]) != "[]" || string(data["env_from"]) != "[]" {
				t.Fatal(data)
			}
			var names, from []string
			var ciphertext *string
			if err := pool.QueryRow(t.Context(), `SELECT env_names,env_from,env_ciphertext FROM runs WHERE id=$1`, rid).Scan(&names, &from, &ciphertext); err != nil || len(names) != 0 || len(from) != 0 || ciphertext != nil {
				t.Fatal(names, from, ciphertext, err)
			}
		} else if len(data) != 2 {
			t.Fatal(data)
		}
	}
	for range 2 {
		if _, err := p.Up(t.Context()); err != nil {
			t.Fatal(err)
		}
		check(true)
		if _, err := p.DownTo(t.Context(), 5); err != nil {
			t.Fatal(err)
		}
		check(false)
	}
}
