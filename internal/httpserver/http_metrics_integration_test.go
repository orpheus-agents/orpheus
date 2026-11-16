//go:build integration

package httpserver

import (
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestHTTPMetricsDatabaseOutage(t *testing.T) {
	_, storage := testServer(t)
	metrics := NewHTTPMetrics()
	api, err := Handler(storage, t.Context(), metrics)
	if err != nil {
		t.Fatal(err)
	}
	system := SystemHandler(new(atomic.Bool), metrics, storage.AccountLimits)
	storage.Pool.Close()
	r := httptest.NewRequest("GET", "/api/v1/sessions", nil)
	r.Header.Set("Authorization", "Bearer key")
	w := httptest.NewRecorder()
	api.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatal(w.Code, w.Body.String())
	}
	for path, status := range map[string]int{"/metrics": 200, "/metrics/service": 503} {
		w = httptest.NewRecorder()
		system.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != status {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	want := requestMetricsHelp + `orpheus_http_requests_total{method="GET",route="/api/v1/sessions",status="503"} 1` + "\n"
	if err := testutil.GatherAndCompare(metrics.registry, strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}
}
