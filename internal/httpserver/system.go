package httpserver

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"

	"github.com/orpheus-agents/orpheus/internal/store"
)

// SystemHandler exposes process probes and database-backed service metrics.
// Probes do not consult external dependencies.
// The serving lifecycle owns ready; this handler is never mounted on the API.
func SystemHandler(ready *atomic.Bool, readLimits func(context.Context) (store.LimitReport, error)) http.Handler {
	mux := http.NewServeMux()
	probe := func(w http.ResponseWriter, available bool) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if !available {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "{\"status\":\"unavailable\"}\n")
			return
		}
		_, _ = io.WriteString(w, "{\"status\":\"ok\"}\n")
	}
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		probe(w, true)
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, _ *http.Request) {
		probe(w, ready.Load())
	})
	mux.Handle("GET /metrics/service", serviceMetricsHandler(readLimits))
	return mux
}
