package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/orpheus-agents/orpheus/internal/accountlimits"
	"github.com/orpheus-agents/orpheus/internal/store"
)

func TestServiceMetricsContract(t *testing.T) {
	observed, reset := time.Unix(1000, 0), time.Unix(2000, 0)
	m := serviceMetrics{report: store.LimitReport{Items: []store.LimitItem{
		{AccountID: "team", State: "stale", ObservedAt: &observed, ResetCreditsAvailable: new(int64(0)), Profiles: []string{"private-profile"}, ErrorCode: new("temporarily_unavailable"), Buckets: []accountlimits.Bucket{
			{LimitID: "codex", Primary: &accountlimits.Window{RemainingPercent: 7, ResetsAt: &reset}, Secondary: &accountlimits.Window{RemainingPercent: 0}},
			{LimitID: "empty"},
		}},
		{AccountID: "unknown", State: "unknown"},
		{AccountID: "unavailable", State: "unavailable"},
		{AccountID: "fresh", State: "fresh", ResetCreditsAvailable: new(int64(2))},
	}}}
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(m)
	want := `
# HELP orpheus_account_limit_remaining_percent Last observed remaining quota percentage; use with account freshness.
# TYPE orpheus_account_limit_remaining_percent gauge
orpheus_account_limit_remaining_percent{account_id="team",limit_id="codex",window="primary"} 7
orpheus_account_limit_remaining_percent{account_id="team",limit_id="codex",window="secondary"} 0
# HELP orpheus_account_limit_state Account observation state: exactly one state is 1.
# TYPE orpheus_account_limit_state gauge
orpheus_account_limit_state{account_id="team",state="fresh"} 0
orpheus_account_limit_state{account_id="team",state="stale"} 1
orpheus_account_limit_state{account_id="team",state="unknown"} 0
orpheus_account_limit_state{account_id="team",state="unavailable"} 0
orpheus_account_limit_state{account_id="unknown",state="fresh"} 0
orpheus_account_limit_state{account_id="unknown",state="stale"} 0
orpheus_account_limit_state{account_id="unknown",state="unknown"} 1
orpheus_account_limit_state{account_id="unknown",state="unavailable"} 0
orpheus_account_limit_state{account_id="unavailable",state="fresh"} 0
orpheus_account_limit_state{account_id="unavailable",state="stale"} 0
orpheus_account_limit_state{account_id="unavailable",state="unknown"} 0
orpheus_account_limit_state{account_id="unavailable",state="unavailable"} 1
orpheus_account_limit_state{account_id="fresh",state="fresh"} 1
orpheus_account_limit_state{account_id="fresh",state="stale"} 0
orpheus_account_limit_state{account_id="fresh",state="unknown"} 0
orpheus_account_limit_state{account_id="fresh",state="unavailable"} 0
# HELP orpheus_account_limit_observed_timestamp_seconds Unix timestamp of the last successful account observation.
# TYPE orpheus_account_limit_observed_timestamp_seconds gauge
orpheus_account_limit_observed_timestamp_seconds{account_id="team"} 1000
# HELP orpheus_account_limit_reset_timestamp_seconds Last observed Unix timestamp of the next quota window reset.
# TYPE orpheus_account_limit_reset_timestamp_seconds gauge
orpheus_account_limit_reset_timestamp_seconds{account_id="team",limit_id="codex",window="primary"} 2000
# HELP orpheus_account_reset_credits_available Last observed available earned resets; absent when unknown. Use with account freshness.
# TYPE orpheus_account_reset_credits_available gauge
orpheus_account_reset_credits_available{account_id="team"} 0
orpheus_account_reset_credits_available{account_id="fresh"} 2
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}
}

func TestServiceMetricsRefreshAndNegotiation(t *testing.T) {
	var ready atomic.Bool
	calls := 0
	report := store.LimitReport{Items: []store.LimitItem{{AccountID: "team", State: "fresh", ResetCreditsAvailable: new(int64(2)), Buckets: []accountlimits.Bucket{{LimitID: "codex", Primary: &accountlimits.Window{RemainingPercent: 7}}}}}}
	handler := SystemHandler(&ready, func(context.Context) (store.LimitReport, error) { calls++; return report, nil })
	scrape := func(accept string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("GET", "/metrics/service", nil)
		r.Header.Set("Accept", accept)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(w.Code, w.Header(), w.Body.String())
		}
		return w
	}
	w := scrape("text/plain; version=0.0.4")
	if !strings.Contains(w.Body.String(), `orpheus_account_reset_credits_available{account_id="team"} 2`) || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatal(w.Header(), w.Body.String())
	}
	report.Items[0].ResetCreditsAvailable = nil
	report.Items[0].Buckets = nil
	w = scrape("application/openmetrics-text; version=1.0.0")
	if strings.Contains(w.Body.String(), "orpheus_account_reset_credits_available") || strings.Contains(w.Body.String(), "orpheus_account_limit_remaining_percent") || !strings.HasSuffix(w.Body.String(), "# EOF\n") || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/openmetrics-text") {
		t.Fatal(w.Header(), w.Body.String())
	}
	report.Items = nil
	w = scrape("text/plain; version=0.0.4")
	if w.Body.Len() != 0 || calls != 3 {
		t.Fatal("empty catalog or extra reads", w.Body.String(), calls)
	}
}

func TestServiceMetricsFailure(t *testing.T) {
	for _, name := range []string{"storage", "timeout", "cancelled", "late success"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if name == "cancelled" {
				cancel()
			}
			handler := serviceMetricsHandler(func(readCtx context.Context) (store.LimitReport, error) {
				if name == "storage" {
					return store.LimitReport{}, errors.New("secret database details")
				}
				deadline, ok := readCtx.Deadline()
				if !ok || time.Until(deadline) > 3*time.Second {
					t.Error("missing read budget")
				}
				if name == "late success" {
					cancel()
					return store.LimitReport{}, nil
				}
				<-readCtx.Done()
				return store.LimitReport{}, readCtx.Err()
			})
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequestWithContext(ctx, "GET", "/metrics/service", nil))
			if w.Code != http.StatusServiceUnavailable || w.Body.String() != "Service metrics are temporarily unavailable.\n" {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
}

func TestServiceMetricsConcurrentSnapshots(t *testing.T) {
	var calls atomic.Int64
	handler := serviceMetricsHandler(func(context.Context) (store.LimitReport, error) {
		id := calls.Add(1)
		return store.LimitReport{Items: []store.LimitItem{{AccountID: fmt.Sprint(id), State: "fresh", ResetCreditsAvailable: &id}}}, nil
	})
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics/service", nil))
			if w.Code != 200 {
				t.Error(w.Code)
				return
			}
			body := w.Body.String()
			if strings.Count(body, "\norpheus_account_reset_credits_available{") != 1 || strings.Count(body, "\norpheus_account_limit_state{") != 4 {
				t.Error("mixed snapshots", body)
			}
			for _, forbidden := range []string{"go_", "process_", "promhttp_"} {
				if strings.Contains(body, forbidden) {
					t.Error("non-service metrics", body)
				}
			}
		})
	}
	wg.Wait()
	if calls.Load() != 20 {
		t.Fatal("extra reads", calls.Load())
	}
}
