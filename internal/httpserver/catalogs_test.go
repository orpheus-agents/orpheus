package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus/client"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store"
)

func TestCatalogsWithoutDatabaseOrCredentials(t *testing.T) {
	// A nil pool and missing credential sources ensure these are local reads.
	storage := &store.Store{Settings: config.DefaultSettings(), Profiles: config.Profiles{
		Profiles: map[string]config.Profile{
			"z-api": {Harness: "codex", Auth: config.Auth{Mode: "api_key", APIKeyEnv: "MISSING_SECRET_KEY"}},
			"a-account": {Description: new("Research"), Harness: "codex", Model: new("model"), Instructions: "Public instructions",
				Codex: config.CodexProfile{Effort: "high", Summary: "auto", Personality: "friendly", ServiceTier: "priority"},
				Auth:  config.Auth{Mode: "account", AccountID: "PRIVATE_ACCOUNT", Store: "private", Key: "PRIVATE_AUTH_PATH"}},
		},
		Templates:        map[string]config.Template{"z": {}, "a:v1.2.0": {Description: new("Tagged sandbox")}},
		CredentialStores: map[string]session.CredentialStore{"private": {Bucket: "PRIVATE_BUCKET"}},
	}}
	storage.Settings.PublicAPIKeys = []string{"key"}
	handler, err := Handler(storage, t.Context())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c, err := client.NewClientWithResponses(server.URL, client.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer key")
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := c.GetProfilesWithResponse(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if profiles.StatusCode() != 200 || profiles.JSON200 == nil {
		t.Fatalf("profiles: %s", profiles.Body)
	}
	wantProfiles := `{"items":[{"name":"a-account","description":"Research","harness":"codex","model":"model","instructions":"Public instructions","codex":{"effort":"high","summary":"auto","personality":"friendly","service_tier":"priority"}},{"name":"z-api","description":null,"harness":"codex","model":null,"instructions":"","codex":{}}]}`
	assertCatalogJSON(t, profiles.Body, wantProfiles)
	templates, err := c.GetTemplatesWithResponse(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if templates.StatusCode() != 200 || templates.JSON200 == nil {
		t.Fatalf("templates: %s", templates.Body)
	}
	assertCatalogJSON(t, templates.Body, `{"items":[{"name":"a:v1.2.0","description":"Tagged sandbox"},{"name":"z","description":null}]}`)
	for _, headers := range []http.Header{profiles.HTTPResponse.Header, templates.HTTPResponse.Header} {
		if headers.Get("Cache-Control") != "no-store" {
			t.Fatal(headers)
		}
	}
	// Anonymous reads also stay available without a database.
	storage.Settings.BrowserAuth.Mode = "anonymous"
	anonymous, err := Handler(storage, t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/profiles", "/api/v1/templates"} {
		w := httptest.NewRecorder()
		anonymous.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	// API DTOs retain arrays when a catalog is empty in memory.
	clear(storage.Profiles.Profiles)
	clear(storage.Profiles.Templates)
	for _, path := range []string{"/api/v1/profiles", "/api/v1/templates"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer key")
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || response.StatusCode != 200 {
			t.Fatal(err, response.StatusCode)
		}
		assertCatalogJSON(t, body, `{"items":[]}`)
	}
}

func assertCatalogJSON(t *testing.T, raw []byte, expected string) {
	t.Helper()
	var got, want any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(expected), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("catalog:\n%s\nwant:\n%s", raw, expected)
	}
}

func TestCatalogDescriptionNormalization(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		want        *string
	}{
		{"missing", "", nil},
		{"empty", "description = ''", nil},
		{"whitespace", "description = \" \\t\\n\\u00a0\"", nil},
		{"text", "description = '  Research profile  '", new("  Research profile  ")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := "[templates.codex]\n" + tc.field + "\n[profiles.default]\n" + tc.field +
				"\nharness = 'codex'\n[profiles.default.auth]\nmode = 'api_key'\napi_key_env = 'KEY'\n"
			profiles, err := config.ReadProfiles(strings.NewReader(source))
			if err != nil {
				t.Fatal(err)
			}
			storage := &store.Store{Settings: config.DefaultSettings(), Profiles: profiles}
			storage.Settings.BrowserAuth.Mode = "anonymous"
			handler, err := Handler(storage, t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"/api/v1/profiles", "/api/v1/templates"} {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
				if response.Code != http.StatusOK {
					t.Fatal(response.Code, response.Body.String())
				}
				var got struct{ Items []map[string]any }
				if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if len(got.Items) != 1 {
					t.Fatal(got)
				}
				description, exists := got.Items[0]["description"]
				var want any
				if tc.want != nil {
					want = *tc.want
				}
				if !exists || description != want {
					t.Fatalf("%s: description=%v, exists=%v, want=%v", path, description, exists, want)
				}
			}
		})
	}
}
