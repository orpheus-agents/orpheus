//go:build integration

package migrate_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/orpheus-agents/orpheus/internal/migrate"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

func TestSandboxAccessEventMigration(t *testing.T) {
	pool := testutil.Database(t)
	db := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer func() { _ = db.Close() }()
	p, err := migrate.Provider(db, "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(t.Context(), 4); err != nil {
		t.Fatal(err)
	}
	sid := uuid.New()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	original := []byte(`{"state":"paused","last_known_state":"paused","error":null}`)
	if _, err := pool.Exec(t.Context(), `INSERT INTO sessions(id,configuration,next_event_sequence) VALUES($1,'{}',2)`, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO session_events(session_id,sequence,type,created_at,data) VALUES($1,1,'sandbox.updated',$2,$3)`, sid, at, original); err != nil {
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
		_, hasID := data["id"]
		_, hasWorkspace := data["workspace"]
		if hasID != up || hasWorkspace != up || string(data["state"]) != `"paused"` || string(data["last_known_state"]) != `"paused"` || string(data["error"]) != "null" {
			t.Fatal(data)
		}
		if up && (string(data["id"]) != "null" || string(data["workspace"]) != "null") {
			t.Fatal(data)
		}
	}
	for range 2 {
		if _, err := p.Up(t.Context()); err != nil {
			t.Fatal(err)
		}
		check(true)
		if _, err := p.DownTo(t.Context(), 4); err != nil {
			t.Fatal(err)
		}
		check(false)
	}
}
