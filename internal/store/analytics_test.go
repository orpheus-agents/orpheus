//go:build integration

package store

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/analytics"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

func TestAnalyticsOverviewBucketsAndUsage(t *testing.T) {
	s := fixture(t)
	ctx := t.Context()
	to := time.Now().UTC().Add(-time.Hour).Truncate(time.Hour)
	from := to.Add(-2 * time.Hour)
	got, err := s.AnalyticsOverview(ctx, analytics.Request{From: &from, To: &to, Namespace: new("fixture")})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Series) != 2 || got.Period.RunsCount != 0 || got.Current.ActiveSessions != 0 || got.Period.Usage.TotalTokens != "0" {
		t.Fatalf("empty overview: %+v", got)
	}
	for _, row := range []struct {
		status, namespace string
		created           time.Time
		finished          *time.Time
		usage             int64
	}{
		{"completed", "fixture", from, new(to.Add(10 * time.Minute)), 9000000000000000000},
		{"failed", "fixture", from.Add(time.Hour), new(from.Add(time.Hour + 20*time.Minute)), 9000000000000000000},
		{"running", "fixture", from.Add(-time.Hour), nil, 17},
		{"accepted", "fixture", to, nil, 19},
		{"completed", "other", from, new(from.Add(time.Minute)), 23},
	} {
		sid, rid := uuid.New(), uuid.New()
		if _, err := s.Pool.Exec(ctx, `INSERT INTO sessions(id,configuration,namespace,next_run_number,total_tokens) VALUES($1,'{}',$2,2,1)`, sid, row.namespace); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Pool.Exec(ctx, `INSERT INTO runs(id,session_id,number,status,created_at,finished_at,input_tokens,output_tokens,total_tokens) VALUES($1,$2,1,$3,$4,$5,$6,0,$6)`, rid, sid, row.status, row.created, row.finished, row.usage); err != nil {
			t.Fatal(err)
		}
	}
	got, err = s.AnalyticsOverview(ctx, analytics.Request{From: &from, To: &to, Namespace: new("fixture")})
	if err != nil {
		t.Fatal(err)
	}
	if got.Period.RunsCount != 2 || got.Current.ActiveSessions != 2 || got.Period.ByStatus.Completed != 1 || got.Period.ByStatus.Failed != 1 || got.Series[0].RunsCount != 1 || got.Series[1].RunsCount != 1 {
		t.Fatalf("counts: %+v", got)
	}
	if got.Period.Usage.TotalTokens != "18000000000000000000" || got.Period.RuntimeSeconds != 9000 {
		t.Fatalf("usage/runtime: %+v", got.Period)
	}
	if !got.Series[0].From.Equal(from) || !got.Series[1].To.Equal(to) {
		t.Fatalf("boundaries: %+v", got.Series)
	}
}

func TestAnalyticsStatusDistributionAndActiveRuntime(t *testing.T) {
	s := fixture(t)
	ctx := t.Context()
	from := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	to := from.Add(time.Hour)
	for _, status := range []string{"accepted", "starting", "running", "cancelling", "finalizing", "completed", "failed", "cancelled"} {
		sid := uuid.New()
		if _, err := s.Pool.Exec(ctx, `INSERT INTO sessions(id,configuration) VALUES($1,'{}')`, sid); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Pool.Exec(ctx, `INSERT INTO runs(id,session_id,number,status,created_at) VALUES($1,$2,1,$3,$4)`, uuid.New(), sid, status, from.Add(30*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.AnalyticsOverview(ctx, analytics.Request{From: &from, To: &to})
	if err != nil {
		t.Fatal(err)
	}
	if got.Period.RunsCount != 8 || got.Current.ActiveSessions != 5 || got.Period.ByStatus != (analytics.StatusCounts{Accepted: 1, Starting: 1, Running: 1, Cancelling: 1, Finalizing: 1, Completed: 1, Failed: 1, Cancelled: 1}) {
		t.Fatalf("status distribution: %+v", got)
	}
	// Runs without finished_at contribute through the database snapshot time.
	want := 8 * got.AsOf.Sub(from.Add(30*time.Minute)).Seconds()
	if got.Period.RuntimeSeconds < want-1 || got.Period.RuntimeSeconds > want+1 {
		t.Fatalf("active runtime %v, want %v", got.Period.RuntimeSeconds, want)
	}
}

func TestDashboardSessionFiltersAndCursor(t *testing.T) {
	s := fixture(t)
	ctx := t.Context()
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	var ids []uuid.UUID
	for i := range 3 {
		a, err := s.Accept(ctx, request())
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, a.SessionID)
		if _, err := s.Pool.Exec(ctx, "UPDATE sessions SET created_at=$1 WHERE id=$2", base.Add(time.Duration(i)*time.Minute), a.SessionID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Pool.Exec(ctx, "UPDATE runs SET created_at=$1 WHERE id=$2", base.Add(time.Duration(10+i*10)*time.Minute), a.RunID); err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			if _, err := s.Pool.Exec(ctx, "UPDATE runs SET status='completed',finished_at=$1 WHERE id=$2", base.Add(21*time.Minute), a.RunID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Pool.Exec(ctx, "INSERT INTO runs(id,session_id,number,status,created_at) VALUES($1,$2,2,'failed',$3)", uuid.New(), a.SessionID, base.Add(40*time.Minute)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Pool.Exec(ctx, "UPDATE sessions SET next_run_number=3 WHERE id=$1", a.SessionID); err != nil {
				t.Fatal(err)
			}
		}
	}
	filter := ListFilter{Sort: "last_run_created_at", Order: "desc"}
	var ordered []uuid.UUID
	var cursor string
	for {
		page, err := s.ListSessions(ctx, 1, cursor, filter)
		if err != nil {
			t.Fatal(err)
		}
		ordered = append(ordered, page.Items[0].ID)
		if page.Items[0].LastRunCreatedAt.IsZero() {
			t.Fatal("missing last_run_created_at")
		}
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	if len(ordered) != 3 || ordered[0] != ids[1] || ordered[1] != ids[2] || ordered[2] != ids[0] {
		t.Fatal(ordered)
	}
	for _, tc := range []struct {
		activity string
		want     []uuid.UUID
	}{
		{"active", []uuid.UUID{ids[2], ids[0]}},
		{"inactive", []uuid.UUID{ids[1]}},
	} {
		filter.Activity = tc.activity
		page, err := s.ListSessions(ctx, 10, "", filter)
		if err != nil || len(page.Items) != len(tc.want) {
			t.Fatalf("%s: %+v %v", tc.activity, page, err)
		}
		for i, item := range page.Items {
			if item.ID != tc.want[i] {
				t.Fatalf("%s: %+v", tc.activity, page.Items)
			}
		}
	}
	filter.Activity, filter.Status = "active", new("failed")
	empty, err := s.ListSessions(ctx, 10, "", filter)
	if err != nil || len(empty.Items) != 0 {
		t.Fatalf("activity/status intersection: %+v %v", empty, err)
	}
	filter.Activity = "inactive"
	matched, err := s.ListSessions(ctx, 10, "", filter)
	if err != nil || len(matched.Items) != 1 || matched.Items[0].ID != ids[1] {
		t.Fatalf("activity/status intersection: %+v %v", matched, err)
	}
	filter.Status = nil
	end := base.Add(35 * time.Minute)
	filter.Activity = "all"
	filter.LastRunCreatedFrom, filter.LastRunCreatedTo = &base, &end
	page, err := s.ListSessions(ctx, 10, "", filter)
	if err != nil || len(page.Items) != 2 || page.Items[0].ID != ids[2] || page.Items[1].ID != ids[0] {
		t.Fatalf("latest range: %+v %v", page, err)
	}
	filter.LastRunCreatedFrom, filter.LastRunCreatedTo = nil, nil
	first, err := s.ListSessions(ctx, 1, "", filter)
	if err != nil || first.NextCursor == nil {
		t.Fatal(first, err)
	}
	for _, changed := range []ListFilter{{Sort: "created_at"}, {Sort: "last_run_created_at", Activity: "inactive"}, {Sort: "last_run_created_at", LastRunCreatedFrom: &base, LastRunCreatedTo: &end}} {
		_, err := s.ListSessions(ctx, 1, *first.NextCursor, changed)
		if p, ok := errors.AsType[*session.APIError](err); !ok || p.Problem.Code != "invalid_cursor" {
			t.Fatalf("cursor scope: %v", err)
		}
	}
	// Lists are live: a new run can move a session before the saved cursor.
	if _, err := s.Pool.Exec(ctx, "UPDATE runs SET status='completed',finished_at=$1 WHERE session_id=$2 AND number=1", base.Add(31*time.Minute), ids[2]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, "INSERT INTO runs(id,session_id,number,status,created_at) VALUES($1,$2,2,'accepted',$3)", uuid.New(), ids[2], base.Add(50*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, "UPDATE sessions SET next_run_number=3 WHERE id=$1", ids[2]); err != nil {
		t.Fatal(err)
	}
	continued, err := s.ListSessions(ctx, 10, *first.NextCursor, filter)
	if err != nil || len(continued.Items) != 1 || continued.Items[0].ID != ids[0] {
		t.Fatalf("live cursor continuation: %+v %v", continued, err)
	}
	fresh, err := s.ListSessions(ctx, 10, "", filter)
	if err != nil || len(fresh.Items) != 3 || fresh.Items[0].ID != ids[2] || fresh.Items[1].ID != ids[1] {
		t.Fatalf("fresh traversal: %+v %v", fresh, err)
	}
}

func TestAnalyticsSnapshotConcurrentRunUpdate(t *testing.T) {
	s := fixture(t)
	ctx := t.Context()
	from := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour)
	to := from.Add(time.Hour)
	sid, rid := uuid.New(), uuid.New()
	if _, err := s.Pool.Exec(ctx, `INSERT INTO sessions(id,configuration) VALUES($1,'{}')`, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO runs(id,session_id,number,status,created_at) VALUES($1,$2,1,'running',$3)`, rid, sid, from.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	err := s.Snapshot(ctx, func(tx pgx.Tx) error {
		asOf, err := db.New(tx).AnalyticsAsOf(ctx)
		if err != nil {
			return err
		}
		if _, err := s.Pool.Exec(ctx, `UPDATE runs SET status='completed',finished_at=$2,total_tokens=42 WHERE id=$1`, rid, asOf); err != nil {
			return err
		}
		active, err := db.New(tx).AnalyticsActiveSessions(ctx, nil)
		if err != nil {
			return err
		}
		if active != 1 {
			t.Fatalf("mixed snapshot: active=%d", active)
		}
		rows, err := tx.Query(ctx, analyticsBucketsSQL, "hour", from, to, "UTC", asOf, nil)
		if err != nil {
			return err
		}
		defer rows.Close()
		var seen int
		for rows.Next() {
			values, err := rows.Values()
			if err != nil {
				return err
			}
			if values[2].(int64) == 1 {
				seen++
				if values[5].(int64) != 1 || values[8].(int64) != 0 || values[13].(string) != "0" {
					t.Fatalf("mixed snapshot bucket: %v", values)
				}
			}
		}
		if seen != 1 {
			t.Fatalf("run in %d buckets", seen)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err := s.AnalyticsOverview(ctx, analytics.Request{From: &from, To: &to})
	if err != nil || after.Current.ActiveSessions != 0 || after.Period.ByStatus.Completed != 1 || after.Period.Usage.TotalTokens != "42" {
		t.Fatalf("new snapshot: %+v %v", after, err)
	}
}

func TestAnalyticsDSTAndValidation(t *testing.T) {
	s := fixture(t)
	for _, tc := range []struct {
		date  string
		hours int
	}{
		{"2025-03-30", 23}, {"2025-10-26", 25},
	} {
		loc, err := time.LoadLocation("Europe/Berlin")
		if err != nil {
			t.Fatal(err)
		}
		day, err := time.ParseInLocation("2006-01-02", tc.date, loc)
		if err != nil {
			t.Fatal(err)
		}
		from, to := day.UTC(), day.AddDate(0, 0, 1).UTC()
		got, err := s.AnalyticsOverview(t.Context(), analytics.Request{From: &from, To: &to, Bucket: "hour", Timezone: "Europe/Berlin"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Series) != tc.hours || !got.Series[0].From.Equal(from) || !got.Series[len(got.Series)-1].To.Equal(to) {
			t.Fatalf("%s: %d buckets; endpoints %v..%v, want %v..%v", tc.date, len(got.Series), got.Series[0].From, got.Series[len(got.Series)-1].To, from, to)
		}
		for i := 1; i < len(got.Series); i++ {
			if !got.Series[i-1].To.Equal(got.Series[i].From) {
				t.Fatalf("gap at %d: %+v", i, got.Series)
			}
		}
		localTwo := 0
		for _, bucket := range got.Series {
			if bucket.From.In(loc).Hour() == 2 {
				localTwo++
			}
		}
		if tc.hours == 23 && localTwo != 0 || tc.hours == 25 && localTwo != 2 {
			t.Fatalf("%s: local 02:00 occurs %d times", tc.date, localTwo)
		}
		dayBuckets, err := s.AnalyticsOverview(t.Context(), analytics.Request{From: &from, To: &to, Bucket: "day", Timezone: "Europe/Berlin"})
		if err != nil || len(dayBuckets.Series) != 1 {
			t.Fatalf("%s: daily bucket: %+v %v", tc.date, dayBuckets.Series, err)
		}
		if got := dayBuckets.Series[0].To.Sub(dayBuckets.Series[0].From); got != time.Duration(tc.hours)*time.Hour {
			t.Fatalf("%s: daily bucket is %s, want %d hours", tc.date, got, tc.hours)
		}
	}
	partialFrom := time.Date(2025, 10, 26, 0, 30, 0, 0, time.UTC)
	partialTo := partialFrom.Add(2*time.Hour + 15*time.Minute)
	partial, err := s.AnalyticsOverview(t.Context(), analytics.Request{From: &partialFrom, To: &partialTo, Timezone: "Europe/Moscow"})
	if err != nil || len(partial.Series) != 3 || !partial.Series[0].From.Equal(partialFrom) || !partial.Series[2].To.Equal(partialTo) {
		t.Fatalf("partial buckets: %+v %v", partial, err)
	}
	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	for _, tc := range []struct {
		request analytics.Request
		path    string
	}{
		{analytics.Request{From: &from}, "to"},
		{analytics.Request{From: &from, To: &to}, "to"},
		{analytics.Request{Timezone: "Not/AZone"}, "timezone"},
		{analytics.Request{Window: "7d", From: &from, To: &to}, "window"},
	} {
		_, err := s.AnalyticsOverview(t.Context(), tc.request)
		problem, ok := errors.AsType[*session.APIError](err)
		if !ok || problem.Status != 422 || problem.Problem.Code != "validation_error" || len(problem.Problem.Details) != 1 || len(problem.Problem.Details[0].Path) != 2 || problem.Problem.Details[0].Path[0] != "query" || problem.Problem.Details[0].Path[1] != tc.path {
			t.Fatalf("expected query.%s validation error, got %v", tc.path, err)
		}
	}
}
