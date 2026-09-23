//go:build integration

package migrate_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/orpheus-agents/orpheus/internal/migrate"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

func TestTokenUsageMigrations(t *testing.T) {
	pool := testutil.Database(t)
	db := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer func() { _ = db.Close() }()
	p, err := migrate.Provider(db, "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	sid, rid := uuid.New(), uuid.New()
	at := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(t.Context(), `INSERT INTO sessions(id,configuration) VALUES($1,'{"public":{"limits":{"run_timeout_seconds":3600}}}')`, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO runs(id,session_id,number,status) VALUES($1,$2,1,'completed')`, rid, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO session_events(session_id,sequence,type,created_at,data) VALUES($1,1,'run.updated',$2,'{"status":"completed","stop_reason":null}')`, sid, at); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := p.Up(t.Context()); err != nil {
			t.Fatal(err)
		}
		var limit, total int64
		if err := pool.QueryRow(t.Context(), `SELECT (configuration#>>'{public,limits,max_session_tokens}')::bigint,total_tokens FROM sessions WHERE id=$1`, sid).Scan(&limit, &total); err != nil || limit != 100000000 || total != 0 {
			t.Fatal(limit, total, err)
		}
		var usage int64
		var date time.Time
		if err := pool.QueryRow(t.Context(), `SELECT (data#>>'{usage,total_tokens}')::bigint,created_at FROM session_events WHERE session_id=$1`, sid).Scan(&usage, &date); err != nil || usage != 0 || !date.Equal(at) {
			t.Fatal(usage, date, err)
		}
		if _, err := pool.Exec(t.Context(), `UPDATE sessions SET input_tokens=-1 WHERE id=$1`, sid); err == nil {
			t.Fatal("negative usage accepted")
		}
		if _, err := pool.Exec(t.Context(), `UPDATE runs SET stop_reason='token_limit',total_tokens=10 WHERE id=$1`, rid); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `UPDATE session_events SET data=jsonb_set(data,'{stop_reason}','"token_limit"') WHERE session_id=$1`, sid); err != nil {
			t.Fatal(err)
		}
		if _, err := p.DownTo(t.Context(), 10); err != nil {
			t.Fatal(err)
		}
		var changed bool
		if err := pool.QueryRow(t.Context(), `SELECT configuration#>'{public,limits,max_session_tokens}' IS NOT NULL FROM sessions WHERE id=$1`, sid).Scan(&changed); err != nil || changed {
			t.Fatal(changed, err)
		}
		if err := pool.QueryRow(t.Context(), `SELECT data ? 'usage' OR data->>'stop_reason' IS NOT NULL FROM session_events WHERE session_id=$1`, sid).Scan(&changed); err != nil || changed {
			t.Fatal(changed, err)
		}
		var reason *string
		if err := pool.QueryRow(t.Context(), `SELECT stop_reason FROM runs WHERE id=$1`, rid).Scan(&reason); err != nil || reason != nil {
			t.Fatal(reason, err)
		}
	}
}
