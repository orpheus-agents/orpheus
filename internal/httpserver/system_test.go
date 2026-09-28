package httpserver

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestSystemHandler(t *testing.T) {
	var ready atomic.Bool
	handler := SystemHandler(&ready)
	for _, available := range []bool{false, true, false} {
		ready.Store(available)
		for _, path := range []string{"/health", "/ready"} {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			status, body := 200, "{\"status\":\"ok\"}\n"
			if path == "/ready" && !available {
				status, body = 503, "{\"status\":\"unavailable\"}\n"
			}
			if response.Code != status || response.Body.String() != body {
				t.Fatalf("ready=%v %s: %d %s", available, path, response.Code, response.Body.String())
			}
			if response.Header().Get("Content-Type") != "application/json" || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal(response.Header())
			}
		}
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{{"POST", "/ready", 405}, {"POST", "/health", 405}, {"GET", "/health/extra", 404}, {"GET", "/api/v1/sessions", 404}, {"GET", "/openapi.json", 404}, {"GET", "/auth/login", 404}} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))
		if response.Code != tc.status {
			t.Fatalf("%s %s: %d", tc.method, tc.path, response.Code)
		}
	}
}
