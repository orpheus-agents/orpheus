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
	}{
		{name: "default", from: asOf.Add(-24 * time.Hour), to: asOf},
		{name: "seven days", request: Request{Window: "7d", Bucket: "day", Timezone: "Europe/Moscow"}, from: asOf.Add(-7 * 24 * time.Hour), to: asOf},
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
		})
	}
}
