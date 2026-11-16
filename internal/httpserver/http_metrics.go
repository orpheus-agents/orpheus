package httpserver

import (
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// HTTPMetrics belongs to one API instance and is shared with its system listener.
type HTTPMetrics struct {
	registry *prometheus.Registry
	requests *prometheus.CounterVec
}

func NewHTTPMetrics() *HTTPMetrics {
	m := &HTTPMetrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "orpheus_http_requests_total",
			Help: "Completed public HTTP requests by method, route template and response status.",
		}, []string{"method", "route", "status"}),
	}
	m.registry.MustRegister(m.requests)
	return m
}

func (m *HTTPMetrics) observe(method, route string, status int) {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
	default:
		method = "OTHER"
	}
	if status == 0 {
		status = http.StatusOK
	}
	m.requests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
}

func (m *HTTPMetrics) handler() http.Handler {
	serve := promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{EnableOpenMetrics: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		serve.ServeHTTP(w, r)
	})
}
