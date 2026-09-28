package httpserver

import (
	"io"
	"net/http"
	"sync/atomic"
)

// SystemHandler exposes process health without consulting external dependencies.
// The serving lifecycle owns ready; this handler is never mounted on the API.
func SystemHandler(ready *atomic.Bool) http.Handler {
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
	return mux
}
