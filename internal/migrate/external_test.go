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

func TestExternalInputsMigrationPreservesHistory(t *testing.T) {
	pool := testutil.Database(t)
	db := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer func() { _ = db.Close() }()
	p, err := migrate.Provider(db, "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	sid, rid, mid := uuid.New(), uuid.New(), uuid.New()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO sessions(id,configuration,next_event_sequence) VALUES($1,'{}',3)`, sid)
	exec(`INSERT INTO runs(id,session_id,number,status) VALUES($1,$2,1,'completed')`, rid, sid)
	exec(`INSERT INTO messages(id,session_id,run_id,role,kind,text,registered_sequence) VALUES($1,$2,$3,'assistant','answer','done',1)`, mid, sid, rid)
	message := map[string]any{"id": mid.String(), "text": "done", "role": "assistant"}
	run := map[string]any{"id": rid.String(), "status": "completed", "final_message": message}
	originals := make([][]byte, 2)
	for i, v := range []map[string]any{message, run} {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		originals[i] = raw
		kind := []string{"message.updated", "run.updated"}[i]
		exec(`INSERT INTO session_events(session_id,sequence,type,created_at,data) VALUES($1,$2,$3,$4,$5)`, sid, i+1, kind, at, raw)
	}
	checkOld := func() {
		t.Helper()
		var cursor int64
		if err := pool.QueryRow(t.Context(), `SELECT next_event_sequence FROM sessions WHERE id=$1`, sid).Scan(&cursor); err != nil || cursor != 3 {
			t.Fatal(cursor, err)
		}
		for i, raw := range originals {
			var same bool
			var created time.Time
			if err := pool.QueryRow(t.Context(), `SELECT data=$3::jsonb,created_at FROM session_events WHERE session_id=$1 AND sequence=$2`, sid, i+1, raw).Scan(&same, &created); err != nil || !same || !created.Equal(at) {
				t.Fatal(same, created, err)
			}
		}
	}
	for range 2 {
		if _, err := p.Up(t.Context()); err != nil {
			t.Fatal(err)
		}
		var nulls bool
		if err := pool.QueryRow(t.Context(), `SELECT s.namespace IS NULL AND s.external_key IS NULL AND r.input_fingerprint IS NULL AND m.external_key IS NULL FROM sessions s JOIN runs r ON r.session_id=s.id JOIN messages m ON m.run_id=r.id WHERE s.id=$1`, sid).Scan(&nulls); err != nil || !nulls {
			t.Fatal(nulls, err)
		}
		var count int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM session_events WHERE session_id=$1 AND ((type='message.updated' AND data->'external_key'='null'::jsonb) OR (type='run.updated' AND data->'input_fingerprint'='null'::jsonb AND data->'final_message'->'external_key'='null'::jsonb))`, sid).Scan(&count); err != nil || count != 2 {
			t.Fatal(count, err)
		}
		if _, err := p.DownTo(t.Context(), 1); err != nil {
			t.Fatal(err)
		}
		checkOld()
	}
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Down also removes actual metadata written after the upgrade, including events.
	exec(`UPDATE sessions SET namespace='redmine',external_key='issue:7' WHERE id=$1`, sid)
	exec(`UPDATE runs SET input_fingerprint='v1' WHERE id=$1`, rid)
	exec(`UPDATE session_events SET data=jsonb_set(data,'{input_fingerprint}','"v1"') WHERE session_id=$1 AND type='run.updated'`, sid)
	if _, err := p.DownTo(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	checkOld()
}
