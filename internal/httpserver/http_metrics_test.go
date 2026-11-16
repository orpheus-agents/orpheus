package httpserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/store"
)

const requestMetricsHelp = "# HELP orpheus_http_requests_total Completed public HTTP requests by method, route template and response status.\n# TYPE orpheus_http_requests_total counter\n"

func metricsAPI(t *testing.T, metrics *HTTPMetrics) http.Handler {
	t.Helper()
	s := &store.Store{Settings: config.DefaultSettings()}
	s.Settings.PublicAPIKeys = []string{"key"}
	s.Settings.MaxRequestBytes = 1024
	handler, err := Handler(s, t.Context(), metrics)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func TestHTTPMetricsBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body, contentType, token, route, metricMethod string
		status                                                            int
	}{
		{name: "success", method: "GET", path: "/api/v1/services", token: "key", route: "/api/v1/services", status: 200},
		{name: "openapi", method: "GET", path: "/openapi.json?private=value", route: "/openapi.json", status: 200},
		{name: "auth session", method: "GET", path: "/api/v1/auth/session", route: "/api/v1/auth/session", status: 200},
		{name: "login", method: "GET", path: "/auth/login", route: "/auth/login", status: 404},
		{name: "metadata", method: "GET", path: "/saml/metadata", route: "/saml/metadata", status: 404},
		{name: "unauthorized", method: "GET", path: "/api/v1/sessions/00000000-0000-0000-0000-000000000001?private=value", route: "/api/v1/sessions/{sid}", status: 401},
		{name: "invalid id", method: "GET", path: "/api/v1/sessions/private-id", token: "key", route: "/api/v1/sessions/{sid}", status: 422},
		{name: "invalid json", method: "POST", path: "/api/v1/sessions", body: "{", contentType: "application/json", token: "key", route: "/api/v1/sessions", status: 400},
		{name: "too large", method: "POST", path: "/api/v1/sessions", body: strings.Repeat("x", 1025), token: "key", route: "/api/v1/sessions", status: 413},
		{name: "media type", method: "POST", path: "/api/v1/sessions", body: "{}", contentType: "text/plain", token: "key", route: "/api/v1/sessions", status: 415},
		{name: "missing body", method: "POST", path: "/api/v1/sessions", token: "key", route: "/api/v1/sessions", status: 422},
		{name: "duplicate query", method: "GET", path: "/api/v1/sessions?limit=1&limit=2", token: "key", route: "/api/v1/sessions", status: 422},
		{name: "unknown route", method: "GET", path: "/private-path?private=value", route: "unmatched", status: 404},
		{name: "unknown API route", method: "GET", path: "/api/v1/private-path", token: "key", route: "unmatched", status: 404},
		{name: "wrong method", method: "PUT", path: "/api/v1/services", token: "key", route: "unmatched", status: 405},
		{name: "custom method", method: "PRIVATE-METHOD", path: "/api/v1/services", token: "key", route: "unmatched", metricMethod: "OTHER", status: 405},
		{name: "public metrics absent", method: "GET", path: "/metrics", route: "unmatched", status: 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metrics := NewHTTPMetrics()
			handler := metricsAPI(t, metrics)
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", tc.contentType)
			if tc.token != "" {
				r.Header.Set("Authorization", "Bearer "+tc.token)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			method := tc.method
			if tc.metricMethod != "" {
				method = tc.metricMethod
			}
			want := requestMetricsHelp + fmt.Sprintf("orpheus_http_requests_total{method=%q,route=%q,status=%q} 1\n", method, tc.route, fmt.Sprint(tc.status))
			if err := testutil.GatherAndCompare(metrics.registry, strings.NewReader(want)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHTTPMetricsSystemIsolationAndNegotiation(t *testing.T) {
	metrics := NewHTTPMetrics()
	api := metricsAPI(t, metrics)
	api.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/openapi.json", nil))
	system := SystemHandler(new(atomic.Bool), metrics, func(context.Context) (store.LimitReport, error) {
		t.Error("instance metrics read the database")
		return store.LimitReport{}, nil
	})
	for _, path := range []string{"/health", "/ready", "/missing"} {
		system.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}
	for _, format := range []string{"text/plain; version=0.0.4", "application/openmetrics-text; version=1.0.0"} {
		for range 2 {
			r := httptest.NewRequest("GET", "/metrics", nil)
			r.Header.Set("Accept", format)
			w := httptest.NewRecorder()
			system.ServeHTTP(w, r)
			if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(w.Header().Get("Content-Type"), strings.Split(format, ";")[0]) {
				t.Fatal(w.Code, w.Header(), w.Body.String())
			}
			body := w.Body.String()
			if !strings.Contains(body, `orpheus_http_requests_total{method="GET",route="/openapi.json",status="200"} 1`) {
				t.Fatal(body)
			}
			if strings.HasPrefix(format, "application/") && !strings.HasSuffix(body, "# EOF\n") {
				t.Fatal("missing OpenMetrics terminator", body)
			}
			for _, forbidden := range []string{"go_", "process_", "promhttp_", "orpheus_account_"} {
				if strings.Contains(body, forbidden) {
					t.Fatal("unexpected metric", body)
				}
			}
		}
	}
	want := requestMetricsHelp + "orpheus_http_requests_total{method=\"GET\",route=\"/openapi.json\",status=\"200\"} 1\n"
	if err := testutil.GatherAndCompare(metrics.registry, strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPMetricsConcurrentRequestsAndInstances(t *testing.T) {
	metrics := NewHTTPMetrics()
	api := metricsAPI(t, metrics)
	other := NewHTTPMetrics()
	otherAPI := metricsAPI(t, other)
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			for _, id := range []string{"00000000-0000-0000-0000-000000000001", "00000000-0000-0000-0000-000000000002"} {
				w := httptest.NewRecorder()
				api.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/sessions/"+id, nil))
				if w.Code != 401 {
					t.Error(w.Code)
				}
			}
			if _, err := metrics.registry.Gather(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	otherAPI.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/openapi.json", nil))
	for m, want := range map[*HTTPMetrics]string{
		metrics: `orpheus_http_requests_total{method="GET",route="/api/v1/sessions/{sid}",status="401"} 200`,
		other:   `orpheus_http_requests_total{method="GET",route="/openapi.json",status="200"} 1`,
	} {
		if err := testutil.GatherAndCompare(m.registry, strings.NewReader(requestMetricsHelp+want+"\n")); err != nil {
			t.Fatal(err)
		}
	}
	fresh := NewHTTPMetrics()
	if err := testutil.GatherAndCompare(fresh.registry, strings.NewReader("")); err != nil {
		t.Fatal("new instance retained series", err)
	}
}
