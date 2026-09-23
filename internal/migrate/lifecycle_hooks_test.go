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

func TestLifecycleHookMigrations(t *testing.T) {
	pool := testutil.Database(t)
	db := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer func() { _ = db.Close() }()
	p, err := migrate.Provider(db, "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	sid, rid := uuid.New(), uuid.New()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := pool.Exec(t.Context(), `INSERT INTO sessions(id,configuration,next_event_sequence) VALUES($1,'{"public":{"limits":{"run_timeout_seconds":3600}}}',2)`, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO runs(id,session_id,number) VALUES($1,$2,1)`, rid, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO session_events(session_id,sequence,type,created_at,data) VALUES($1,1,'run.updated',$2,$3)`, sid, at, []byte(`{"id":"`+rid.String()+`","status":"accepted"}`)); err != nil {
		t.Fatal(err)
	}
	check := func(up bool) {
		t.Helper()
		var raw, configRaw []byte
		var created time.Time
		if err := pool.QueryRow(t.Context(), `SELECT data,created_at FROM session_events WHERE session_id=$1`, sid).Scan(&raw, &created); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(t.Context(), `SELECT configuration FROM sessions WHERE id=$1`, sid).Scan(&configRaw); err != nil {
			t.Fatal(err)
		}
		if !created.Equal(at) {
			t.Fatal("event timestamp changed", created)
		}
		var data, cfg map[string]json.RawMessage
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(configRaw, &cfg); err != nil {
			t.Fatal(err)
		}
		var public map[string]json.RawMessage
		if err := json.Unmarshal(cfg["public"], &public); err != nil {
			t.Fatal(err)
		}
		_, hasPhase := data["phase"]
		_, hasHooks := data["hooks"]
		_, configured := public["hooks"]
		if hasPhase != up || hasHooks != up || configured != up {
			t.Fatal(data, public)
		}
		if up {
			var hooks map[string]int
			if json.Unmarshal(public["hooks"], &hooks) != nil || hooks["timeout_seconds"] != 300 || string(data["phase"]) != "null" || string(data["hooks"]) != "[]" {
				t.Fatal(data, public)
			}
		}
	}
	for range 2 {
		if _, err := p.Up(t.Context()); err != nil {
			t.Fatal(err)
		}
		check(true)
		if _, err := p.DownTo(t.Context(), 7); err != nil {
			t.Fatal(err)
		}
		check(false)
	}
}
