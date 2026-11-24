package analytics

import (
	"errors"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus/internal/session"
)

func TestRequestResolve(t *testing.T) {
	asOf := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		request Request
		from    time.Time
		to      time.Time
		path    string
		bucket  string
	}{
		{name: "default", from: asOf.Add(-24 * time.Hour), to: asOf},
		{name: "seven days", request: Request{Window: "7d", Bucket: "day", Timezone: "Europe/Moscow"}, from: asOf.Add(-7 * 24 * time.Hour), to: asOf},
		{name: "three months", request: Request{Window: "3m"}, from: asOf.AddDate(0, -3, 0), to: asOf, bucket: "day"},
		{name: "six months", request: Request{Window: "6m", Bucket: "day"}, from: asOf.AddDate(0, -6, 0), to: asOf, bucket: "day"},
		{name: "one year", request: Request{Window: "1y"}, from: asOf.AddDate(-1, 0, 0), to: asOf, bucket: "day"},
		{name: "hourly year", request: Request{Window: "1y", Bucket: "hour"}, path: "bucket"},
		{name: "explicit range", request: Request{From: new(asOf.Add(-2 * time.Hour)), To: &asOf}, from: asOf.Add(-2 * time.Hour), to: asOf},
		{name: "missing to", request: Request{From: new(asOf.Add(-time.Hour))}, path: "to"},
		{name: "missing from", request: Request{To: &asOf}, path: "from"},
		{name: "mixed window", request: Request{Window: "24h", From: new(asOf.Add(-time.Hour)), To: &asOf}, path: "window"},
		{name: "future to", request: Request{From: &asOf, To: new(asOf.Add(time.Second))}, path: "to"},
		{name: "long range", request: Request{From: new(asOf.Add(-32 * 24 * time.Hour)), To: &asOf}, path: "from"},
		{name: "invalid bucket", request: Request{Bucket: "week"}, path: "bucket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.request.Resolve(asOf)
			if tc.path != "" {
				problem, ok := errors.AsType[*session.APIError](err)
				if !ok || problem.Status != 422 || problem.Problem.Code != "validation_error" || len(problem.Problem.Details) != 1 || len(problem.Problem.Details[0].Path) != 2 || problem.Problem.Details[0].Path[1] != tc.path {
					t.Fatalf("want query.%s error, got %v", tc.path, err)
				}
				return
			}
			if err != nil || !got.From.Equal(tc.from) || !got.To.Equal(tc.to) {
				t.Fatalf("resolved %+v, err %v", got, err)
			}
			if tc.bucket != "" && got.Bucket != tc.bucket {
				t.Fatalf("bucket %s, want %s", got.Bucket, tc.bucket)
			}
		})
	}
}

func TestCalendarWindowClampsToLastDay(t *testing.T) {
	for _, tc := range []struct{ window, to, from string }{
		{"3m", "2026-05-31T23:45:00Z", "2026-02-28T23:45:00Z"},
		{"6m", "2024-08-31T23:45:00Z", "2024-02-29T23:45:00Z"},
		{"1y", "2024-02-29T23:45:00Z", "2023-02-28T23:45:00Z"},
	} {
		t.Run(tc.window, func(t *testing.T) {
			to, err := time.Parse(time.RFC3339, tc.to)
			if err != nil {
				t.Fatal(err)
			}
			q, err := (Request{Window: tc.window}).Resolve(to)
			if err != nil || q.From.Format(time.RFC3339) != tc.from {
				t.Fatalf("from %s, want %s (error %v)", q.From, tc.from, err)
			}
		})
	}
}
