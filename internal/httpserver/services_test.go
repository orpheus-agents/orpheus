package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/store"
)

func TestServicesCatalog(t *testing.T) {
	s := &store.Store{Settings: config.DefaultSettings(), Profiles: config.Profiles{Services: map[string]config.Service{
		"z": {Name: "Last", Description: "Last service", EnvFrom: []string{"Z", "A"}},
		"a": {Name: "First", Description: "First service", EnvFrom: []string{"A"}},
	}}}
	s.Settings.PublicAPIKeys = []string{"key"}
	s.Settings.BrowserAuth.Mode = "anonymous"
	handler, err := Handler(s, t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, auth := range []string{"", "Bearer key", "Bearer invalid"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/services", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if auth == "Bearer invalid" {
			if w.Code != 401 {
				t.Fatal(w.Code)
			}
			continue
		}
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(w.Code, w.Body.String())
		}
		assertCatalogJSON(t, w.Body.Bytes(), `{"items":[{"code":"a","name":"First","description":"First service","env_from":["A"]},{"code":"z","name":"Last","description":"Last service","env_from":["A","Z"]}]}`)
	}
	clear(s.Profiles.Services)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/services", nil))
	assertCatalogJSON(t, w.Body.Bytes(), `{"items":[]}`)
}
