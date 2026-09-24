//go:build integration

package httpserver

import (
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus/internal/api"
)

func TestAnalyticsHTTPContract(t *testing.T) {
	server, _ := testServer(t)
	status, headers, body := requestHTTP(t, server, "GET", "/api/v1/analytics/overview?window=7d&bucket=day&timezone=Europe%2FMoscow", "", "key", "")
	if status != 200 || headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("overview: %d %s", status, body)
	}
	var overview api.AnalyticsOverview
	if err := json.Unmarshal(body, &overview); err != nil {
		t.Fatal(err)
	}
	if overview.Period.RunsCount != 0 || overview.Current.ActiveSessions != 0 || len(overview.Series) < 7 || overview.Timezone != "Europe/Moscow" {
		t.Fatalf("empty overview: %+v", overview)
	}
	for i := 1; i < len(overview.Series); i++ {
		if !overview.Series[i-1].To.Equal(overview.Series[i].From) {
			t.Fatalf("bucket gap at %d", i)
		}
	}
	from := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)
	to := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	for _, tc := range []struct {
		query, path string
	}{
		{"window=24h&window=7d", "window"},
		{"window=7d&from=" + url.QueryEscape(from) + "&to=" + url.QueryEscape(to), "window"},
		{"from=" + url.QueryEscape(from), "to"},
		{"from=" + url.QueryEscape(from) + "&to=" + url.QueryEscape(future), "to"},
		{"timezone=Unknown%2FZone", "timezone"},
		{"timezone=", "timezone"},
		{"bucket=week", "bucket"},
	} {
		status, _, body := requestHTTP(t, server, "GET", "/api/v1/analytics/overview?"+tc.query, "", "key", "")
		if status != 422 {
			t.Fatalf("%s: status %d: %s", tc.query, status, body)
		}
		var problem struct {
			Error struct {
				Code    string `json:"code"`
				Details []struct {
					Path []any `json:"path"`
				} `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &problem); err != nil {
			t.Fatal(err)
		}
		if problem.Error.Code != "validation_error" || len(problem.Error.Details) == 0 || len(problem.Error.Details[0].Path) < 2 || problem.Error.Details[0].Path[1] != tc.path {
			t.Fatalf("%s: %s", tc.query, body)
		}
	}
}
