//go:build integration && perf

package store

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus/internal/analytics"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

// Run explicitly with -tags 'integration perf'; never part of ordinary CI.
func TestAnalyticsLargeArchive(t *testing.T) {
	s := fixture(t)
	ctx := t.Context()
	for _, statement := range []string{
		`INSERT INTO sessions(id,configuration,namespace,next_run_number)
         SELECT md5('analytics-session:'||i)::uuid,'{}',
                CASE WHEN i % 100 = 0 THEN 'rare' ELSE 'popular' END,11
         FROM generate_series(1,100000) AS i`,
		`INSERT INTO runs(id,session_id,number,status,created_at,finished_at,input_tokens,output_tokens,total_tokens)
         SELECT md5('analytics-run:'||i||':'||n)::uuid,
                md5('analytics-session:'||i)::uuid,n,
                CASE WHEN n = 10 AND i % 5000 = 0 THEN 'running' ELSE 'completed' END,
                now() - ((i+n) % 720) * interval '1 hour',
                CASE WHEN n = 10 AND i % 5000 = 0 THEN NULL ELSE now() - ((i+n) % 720) * interval '1 hour' + interval '10 minutes' END,
                100,20,120
         FROM generate_series(1,100000) AS i CROSS JOIN generate_series(1,10) AS n`,
		`ANALYZE sessions`, `ANALYZE runs`,
	} {
		start := time.Now()
		if _, err := s.Pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
		t.Logf("fixture statement: %s: %s", statement[:min(len(statement), 40)], time.Since(start))
	}
	for _, tc := range []struct {
		name, window, bucket string
		namespace            *string
	}{
		{"24h all", "24h", "hour", nil},
		{"30d all", "30d", "day", nil},
		{"30d popular", "30d", "day", new("popular")},
		{"30d rare", "30d", "day", new("rare")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			result, err := s.AnalyticsOverview(ctx, analytics.Request{Window: tc.window, Bucket: tc.bucket, Namespace: tc.namespace})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %s, runs=%d, buckets=%d", tc.name, time.Since(start), result.Period.RunsCount, len(result.Series))
		})
	}
	for _, tc := range []struct {
		name, activity string
	}{
		{"active", "active"}, {"inactive", "inactive"},
	} {
		t.Run(tc.name+" sessions", func(t *testing.T) {
			start := time.Now()
			page, err := s.ListSessions(ctx, 50, "", ListFilter{Activity: tc.activity, Sort: "last_run_created_at", Order: "desc"})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %s, page=%d", tc.name, time.Since(start), len(page.Items))
		})
	}
	to := time.Now().UTC()
	from := to.Add(-30 * 24 * time.Hour)
	var raw []byte
	if err := s.Pool.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+analyticsBucketsSQL, "day", from, to, "UTC", to, nil).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plans []struct {
		ExecutionTime float64        `json:"Execution Time"`
		Plan          map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatal(err)
	}
	t.Logf("30d SQL execution: %.1f ms, indexes=%v", plans[0].ExecutionTime, planIndexes(plans[0].Plan))
	e := &explainQueries{DBTX: s.Pool, t: t}
	if _, err := db.New(e).ListSessionsByLatestDesc(ctx, db.ListSessionsByLatestDescParams{Activity: "active", FirstPage: true, PageLimit: 51}); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(e.plans[0]), &plans); err != nil {
		t.Fatal(err)
	}
	t.Logf("active list SQL execution: %.1f ms, indexes=%v", plans[0].ExecutionTime, planIndexes(plans[0].Plan))
}

func planIndexes(root map[string]any) []string {
	var names []string
	var walk func(map[string]any)
	walk = func(node map[string]any) {
		if name, ok := node["Index Name"].(string); ok && !slices.Contains(names, name) {
			names = append(names, name)
		}
		if children, ok := node["Plans"].([]any); ok {
			for _, child := range children {
				walk(child.(map[string]any))
			}
		}
	}
	walk(root)
	slices.Sort(names)
	return names
}
