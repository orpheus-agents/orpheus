//go:build integration

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

type explainQueries struct {
	db.DBTX
	t        *testing.T
	plans    []string
	prepared bool
}

func (e *explainQueries) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	var plan string
	if e.prepared {
		if _, err := e.Exec(ctx, "PREPARE reviewed_query AS "+query); err != nil {
			e.t.Fatal(err)
		}
		placeholders := make([]string, len(args))
		for i := range args {
			placeholders[i] = fmt.Sprintf("$%d", i+1)
		}
		bind := append([]any{pgx.QueryExecModeSimpleProtocol}, args...)
		if err := e.DBTX.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) EXECUTE reviewed_query("+strings.Join(placeholders, ",")+")", bind...).Scan(&plan); err != nil {
			e.t.Fatal(err)
		}
		if _, err := e.Exec(ctx, "DEALLOCATE reviewed_query"); err != nil {
			e.t.Fatal(err)
		}
	} else if err := e.DBTX.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+query, args...).Scan(&plan); err != nil {
		e.t.Fatal(err)
	}
	e.plans = append(e.plans, plan)
	return e.DBTX.Query(ctx, query, args...)
}

func TestExternalQueriesUseIndexes(t *testing.T) {
	s := fixture(t)
	ctx := t.Context()
	for _, sql := range []string{
		`INSERT INTO sessions(id,configuration,namespace,external_key) SELECT md5(i::text)::uuid,'{}',CASE WHEN i<=2 THEN 'rare' ELSE 'integration' END,'issue:'||i FROM generate_series(1,50000) i`,
		`INSERT INTO runs(id,session_id,number,status,input_fingerprint) SELECT id,id,1,CASE WHEN namespace='rare' THEN 'running' ELSE 'completed' END,external_key FROM sessions`,
		`INSERT INTO messages(id,session_id,run_id,role,text,delivery_status,registered_sequence,external_key) SELECT id,id,id,'user','hello','delivered',1,external_key FROM sessions`,
		`INSERT INTO session_events(session_id,sequence,type,data) SELECT id,1,'message.updated',jsonb_build_object('id',id,'run_id',id,'registered_sequence','1','position',NULL) FROM sessions`,
		`INSERT INTO session_events(session_id,sequence,type,data) SELECT md5('1111')::uuid,i,'message.updated',jsonb_build_object('id',md5(('other:'||i)::text)::uuid,'run_id',md5('1111')::uuid,'registered_sequence',i::text,'position',NULL) FROM generate_series(2,5000) i`,
		`ANALYZE sessions`, `ANALYZE runs`, `ANALYZE messages`, `ANALYZE session_events`,
	} {
		if _, err := s.Pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	e := &explainQueries{DBTX: s.Pool, t: t}
	q := db.New(e)
	sessions, err := q.ListSessions(ctx, db.ListSessionsParams{Activity: "all", Namespace: new("integration"), ExternalKey: new("issue:1111"), FirstPage: true, PageLimit: 2})
	if err != nil || len(sessions) != 1 {
		t.Fatal(sessions, err)
	}
	_, err = q.ListAllRuns(ctx, db.ListAllRunsParams{InputFingerprint: new("issue:1111"), FirstPage: true, PageLimit: 2})
	if err != nil {
		t.Fatal(err)
	}
	_, err = q.HistoryByExternalKey(ctx, db.HistoryByExternalKeyParams{SessionID: sessions[0].ID, MessageExternalKey: "issue:1111", Watermark: 5000, FirstPage: true, PageLimit: 2})
	if err != nil {
		t.Fatal(err)
	}
	for i, names := range [][]string{{"sessions_external"}, {"runs_fingerprint"}, {"messages_external", "events_message_snapshot"}} {
		for _, name := range names {
			if !strings.Contains(e.plans[i], name) {
				t.Fatalf("index %s not used: %s", name, e.plans[i])
			}
		}
	}
	for _, test := range []struct {
		name string
		run  func(*db.Queries) error
	}{
		{"sessions status", func(q *db.Queries) error {
			v, err := q.ListSessions(ctx, db.ListSessionsParams{Activity: "all", Status: new("running"), FirstPage: true, PageLimit: 51})
			if err == nil && len(v) != 2 {
				t.Fatal(len(v))
			}
			return err
		}},
		{"sessions namespace status", func(q *db.Queries) error {
			_, err := q.ListSessions(ctx, db.ListSessionsParams{Activity: "all", Namespace: new("rare"), Status: new("running"), FirstPage: true, PageLimit: 51})
			return err
		}},
		{"runs namespace", func(q *db.Queries) error {
			_, err := q.ListAllRuns(ctx, db.ListAllRunsParams{Namespace: new("rare"), FirstPage: true, PageLimit: 51})
			return err
		}},
		{"runs status", func(q *db.Queries) error {
			_, err := q.ListAllRuns(ctx, db.ListAllRunsParams{Status: new("running"), FirstPage: true, PageLimit: 51})
			return err
		}},
		{"sessions desc", func(q *db.Queries) error {
			_, err := q.ListSessionsDesc(ctx, db.ListSessionsDescParams{Activity: "all", FirstPage: true, PageLimit: 51})
			return err
		}},
		{"runs desc", func(q *db.Queries) error {
			_, err := q.ListAllRunsDesc(ctx, db.ListAllRunsDescParams{FirstPage: true, PageLimit: 51})
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := &explainQueries{DBTX: s.Pool, t: t}
			if err := test.run(db.New(e)); err != nil {
				t.Fatal(err)
			}
			assertBoundedPlan(t, e.plans[0])
		})
	}
	t.Run("prepared custom plans", func(t *testing.T) {
		tx, err := s.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		var mode string
		if err := tx.QueryRow(ctx, "SHOW plan_cache_mode").Scan(&mode); err != nil || mode != "force_custom_plan" {
			t.Fatal("unexpected pool plan policy", mode, err)
		}

		warm := db.New(tx)
		for range 6 {
			if _, err := warm.ListSessions(ctx, db.ListSessionsParams{Activity: "all", Namespace: new("integration"), FirstPage: true, PageLimit: 51}); err != nil {
				t.Fatal(err)
			}
			if _, err := warm.ListAllRuns(ctx, db.ListAllRunsParams{Namespace: new("integration"), FirstPage: true, PageLimit: 51}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := warm.ListSessions(ctx, db.ListSessionsParams{Activity: "all", Namespace: new("rare"), FirstPage: true, PageLimit: 51}); err != nil {
			t.Fatal(err)
		}
		if _, err := warm.ListAllRuns(ctx, db.ListAllRunsParams{Namespace: new("rare"), FirstPage: true, PageLimit: 51}); err != nil {
			t.Fatal(err)
		}
		var verified int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_prepared_statements WHERE statement LIKE '-- name: List%' AND custom_plans>=7 AND generic_plans=0`).Scan(&verified); err != nil || verified < 2 {
			t.Fatal("prepared lists did not keep custom plans", verified, err)
		}
		e := &explainQueries{DBTX: tx, t: t, prepared: true}
		q := db.New(e)
		if _, err := q.ListSessionsDesc(ctx, db.ListSessionsDescParams{Activity: "all", FirstPage: true, PageLimit: 51}); err != nil {
			t.Fatal(err)
		}
		if _, err := q.ListAllRunsDesc(ctx, db.ListAllRunsDescParams{FirstPage: true, PageLimit: 51}); err != nil {
			t.Fatal(err)
		}

		for _, ascending := range []bool{false, true} {
			if ascending {
				if _, err := q.ListSessions(ctx, db.ListSessionsParams{Activity: "all", AfterCreatedAt: sessions[0].CreatedAt, AfterID: uuid.MustParse("80000000-0000-0000-0000-000000000000"), PageLimit: 51}); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := q.ListSessionsDesc(ctx, db.ListSessionsDescParams{Activity: "all", AfterCreatedAt: sessions[0].CreatedAt, AfterID: uuid.MustParse("80000000-0000-0000-0000-000000000000"), PageLimit: 51}); err != nil {
					t.Fatal(err)
				}
			}
		}

		for _, ns := range []string{"integration", "rare"} {
			if _, err := q.ListSessions(ctx, db.ListSessionsParams{Activity: "all", Namespace: &ns, FirstPage: true, PageLimit: 51}); err != nil {
				t.Fatal(err)
			}
			if _, err := q.ListAllRuns(ctx, db.ListAllRunsParams{Namespace: &ns, FirstPage: true, PageLimit: 51}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := q.ListSessions(ctx, db.ListSessionsParams{Activity: "all", Status: new("running"), FirstPage: true, PageLimit: 51}); err != nil {
			t.Fatal(err)
		}
		if _, err := q.ListAllRuns(ctx, db.ListAllRunsParams{Status: new("running"), FirstPage: true, PageLimit: 51}); err != nil {
			t.Fatal(err)
		}
		for _, plan := range e.plans {
			assertBoundedPlan(t, plan)
		}
	})

}

func assertBoundedPlan(t *testing.T, raw string) {
	t.Helper()
	var plans []struct{ Plan map[string]any }
	if err := json.Unmarshal([]byte(raw), &plans); err != nil {
		t.Fatal(err)
	}
	var walk func(map[string]any)
	walk = func(node map[string]any) {
		if relation, _ := node["Relation Name"].(string); relation == "sessions" || relation == "runs" {
			rows, _ := node["Actual Rows"].(float64)
			removed, _ := node["Rows Removed by Filter"].(float64)
			loops, _ := node["Actual Loops"].(float64)
			if (rows+removed)*loops > 1000 {
				t.Fatalf("list scanned too much data: %s", raw)
			}
		}
		children, _ := node["Plans"].([]any)
		for _, child := range children {
			walk(child.(map[string]any))
		}
	}
	walk(plans[0].Plan)
}
