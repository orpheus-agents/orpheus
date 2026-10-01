package httpserver

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/orpheus-agents/orpheus/internal/accountlimits"
	"github.com/orpheus-agents/orpheus/internal/store"
)

var (
	limitRemaining = prometheus.NewDesc("orpheus_account_limit_remaining_percent", "Last observed remaining quota percentage; use with account freshness.", []string{"account_id", "limit_id", "window"}, nil)
	limitState     = prometheus.NewDesc("orpheus_account_limit_state", "Account observation state: exactly one state is 1.", []string{"account_id", "state"}, nil)
	limitObserved  = prometheus.NewDesc("orpheus_account_limit_observed_timestamp_seconds", "Unix timestamp of the last successful account observation.", []string{"account_id"}, nil)
	limitReset     = prometheus.NewDesc("orpheus_account_limit_reset_timestamp_seconds", "Last observed Unix timestamp of the next quota window reset.", []string{"account_id", "limit_id", "window"}, nil)
	resetCredits   = prometheus.NewDesc("orpheus_account_reset_credits_available", "Last observed available earned resets; absent when unknown. Use with account freshness.", []string{"account_id"}, nil)
)

// serviceMetrics owns a single read-only database snapshot for one scrape.
// No series survive into the next request, and no provider calls are made here.
type serviceMetrics struct {
	report store.LimitReport
}

func (m serviceMetrics) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{limitRemaining, limitState, limitObserved, limitReset, resetCredits} {
		ch <- desc
	}
}

func (m serviceMetrics) Collect(ch chan<- prometheus.Metric) {
	for _, item := range m.report.Items {
		for _, state := range []string{"fresh", "stale", "unknown", "unavailable"} {
			value := 0.0
			if item.State == state {
				value = 1
			}
			ch <- prometheus.MustNewConstMetric(limitState, prometheus.GaugeValue, value, item.AccountID, state)
		}
		if item.ObservedAt != nil {
			ch <- prometheus.MustNewConstMetric(limitObserved, prometheus.GaugeValue, float64(item.ObservedAt.Unix()), item.AccountID)
		}
		if item.ResetCreditsAvailable != nil {
			ch <- prometheus.MustNewConstMetric(resetCredits, prometheus.GaugeValue, float64(*item.ResetCreditsAvailable), item.AccountID)
		}
		for _, bucket := range item.Buckets {
			collectLimitWindow(ch, item.AccountID, bucket.LimitID, "primary", bucket.Primary)
			collectLimitWindow(ch, item.AccountID, bucket.LimitID, "secondary", bucket.Secondary)
		}
	}
}

func collectLimitWindow(ch chan<- prometheus.Metric, account, bucket, name string, window *accountlimits.Window) {
	if window == nil {
		return
	}
	ch <- prometheus.MustNewConstMetric(limitRemaining, prometheus.GaugeValue, window.RemainingPercent, account, bucket, name)
	if window.ResetsAt != nil {
		ch <- prometheus.MustNewConstMetric(limitReset, prometheus.GaugeValue, float64(window.ResetsAt.Unix()), account, bucket, name)
	}
}

func serviceMetricsHandler(readLimits func(context.Context) (store.LimitReport, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		report, err := readLimits(ctx)
		if err != nil || ctx.Err() != nil {
			http.Error(w, "Service metrics are temporarily unavailable.", http.StatusServiceUnavailable)
			return
		}
		registry := prometheus.NewRegistry()
		registry.MustRegister(serviceMetrics{report: report})
		promhttp.HandlerFor(registry, promhttp.HandlerOpts{EnableOpenMetrics: true}).ServeHTTP(w, r)
	})
}
