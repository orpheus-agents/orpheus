//go:build integration

package httpserver

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/browserauth"
	"github.com/orpheus-agents/orpheus/internal/store/db"
	"github.com/orpheus-agents/orpheus/internal/templates"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

func TestTemplateHTTP(t *testing.T) {
	providerCalls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls++
		if r.Method != "GET" {
			t.Error(r.Method)
		}
		if strings.Contains(r.URL.Path, "/aliases/") {
			_, _ = fmt.Fprint(w, `{"templateID":"provider-id"}`)
		} else {
			_, _ = fmt.Fprint(w, `[{"tag":"default"},{"tag":"v1"}]`)
		}
	}))
	t.Cleanup(provider.Close)
	t.Setenv("AGENTBOX_API_URL", provider.URL)
	t.Setenv("AGENTBOX_API_KEY", "provider-key")
	server, _ := testServer(t)
	empty := externalRequest(t, server, "GET", "/api/v1/templates", "", "", 200)
	assertCatalogJSON(t, empty, `{"items":[],"next_cursor":null,"prev_cursor":null}`)
	raw := externalRequest(t, server, "POST", "/api/v1/templates", `{"template_name":"CODEX","description":" \n "}`, "", 201)
	created := decodeHTTP[templates.Template](t, raw)
	if !created.Active || created.Name != "codex" || created.Tag != nil || created.Description != nil || strings.Contains(string(raw), "provider-id") {
		t.Fatal(string(raw))
	}
	if providerCalls != 2 {
		t.Fatal(providerCalls)
	}
	externalRequest(t, server, "POST", "/api/v1/templates", `{"template_name":"team/codex"}`, "", 409)
	path := "/api/v1/templates/" + created.ID.String()
	for _, body := range []string{`{}`, `{"active":null}`, `{"name":"change"}`, `{"tag":"v1"}`, `{"unknown":1}`} {
		externalRequest(t, server, "PATCH", path, body, "", 422)
	}
	externalRequest(t, server, "PATCH", path, `{"active":true,"active":false}`, "", 400)
	externalRequest(t, server, "POST", "/api/v1/templates", `{"template_name":"codex","active":null}`, "", 422)
	externalRequest(t, server, "POST", "/api/v1/templates", `{"template_name":"codex","tag":"`+uuid.NewString()+`"}`, "", 422)
	for _, query := range []string{"limit=0", "limit=201", "active=bad", "name=", "name_contains=%20", "name=a&name_contains=a", "name=%00", "cursor=bad", "limit=1&limit=2"} {
		externalRequest(t, server, "GET", "/api/v1/templates?"+query, "", "", 422)
	}
	externalRequest(t, server, "GET", "/api/v1/templates/not-a-uuid", "", "", 422)
	externalRequest(t, server, "GET", "/api/v1/templates/"+uuid.NewString(), "", "", 404)
	changed := decodeHTTP[templates.Template](t, externalRequest(t, server, "PATCH", path, `{"active":false,"description":" x "}`, "", 200))
	if changed.Active || *changed.Description != " x " {
		t.Fatal(changed)
	}
	changed = decodeHTTP[templates.Template](t, externalRequest(t, server, "PATCH", path, `{"description":null}`, "", 200))
	if changed.Active || changed.Description != nil {
		t.Fatal(changed)
	}
	externalRequest(t, server, "GET", path, "", "", 200)
	page := decodeHTTP[templates.Page](t, externalRequest(t, server, "GET", "/api/v1/templates?active=false&name="+url.QueryEscape(created.Name), "", "", 200))
	if len(page.Items) != 1 || page.Items[0].ID != created.ID {
		t.Fatal(page)
	}
	// The DB active flag still has no effect on stage-2 core session admission.
	externalRequest(t, server, "POST", "/api/v1/sessions", externalBody, uuid.NewString(), 202)
	externalRequest(t, server, "DELETE", path, "", "", 204)
	externalRequest(t, server, "GET", path, "", "", 404)
	externalRequest(t, server, "DELETE", path, "", "", 404)
	externalRequest(t, server, "PATCH", path, `{"active":true}`, "", 404)
	externalRequest(t, server, "POST", "/api/v1/sessions", externalBody, uuid.NewString(), 202)
	if providerCalls != 4 {
		t.Fatal("read/update/delete called provider", providerCalls)
	}
}

func TestTemplateBrowserWrites(t *testing.T) {
	_, storage := testServer(t)
	fixture := testutil.NewSAML(t, "https://orpheus.example.test")
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	hash := sha256.Sum256([]byte(token))
	if err := db.New(storage.Pool).InsertBrowserSession(t.Context(), db.InsertBrowserSessionParams{TokenHash: hash[:], Subject: "subject", DisplayName: "name", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"anonymous", "saml", "api_only"} {
		storage.Settings.BrowserAuth = fixture.Config
		storage.Settings.BrowserAuth.Mode = mode
		handler, err := Handler(storage, t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name, auth, origin, csrf string
			want                     int
		}{
			{"browser", "", fixture.Config.PublicURL, "1", 404},
			{"wrong origin", "", "https://evil.test", "1", 403},
			{"missing origin", "", "", "1", 403},
			{"missing csrf", "", fixture.Config.PublicURL, "", 403},
			{"bearer", "Bearer key", "", "", 404},
			{"wrong bearer", "Bearer bad", fixture.Config.PublicURL, "1", 401},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				req := httptest.NewRequest("PATCH", "/api/v1/templates/"+uuid.NewString(), strings.NewReader(`{"active":false}`))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Origin", tc.origin)
				req.Header.Set("X-Orpheus-CSRF", tc.csrf)
				req.AddCookie(&http.Cookie{Name: browserauth.CookieName, Value: token})
				if tc.auth != "" {
					req.Header.Set("Authorization", tc.auth)
				}
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, req)
				want := tc.want
				if mode == "api_only" && tc.auth == "" {
					want = 401
				}
				if w.Code != want {
					t.Fatal(w.Code, w.Body.String())
				}
			})
		}
	}
}

func TestTemplateAuditRedactsValues(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/v1/templates", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	audit := newTemplateAudit("CreateTemplate", req)
	audit.identify(req, "saml", nil, nil)
	audit.setFields([]byte(`{"description":"SECRET_DESCRIPTION","active":false,"SECRET_FIELD":"SECRET_VALUE"}`))
	raw, _ := json.Marshal(struct {
		Actor  string
		Fields []string
	}{audit.actor, audit.fields})
	if strings.Contains(string(raw), "secret-key") || strings.Contains(string(raw), "SECRET") || !strings.HasPrefix(audit.actor, "api_key:") || len(audit.fields) != 2 {
		t.Fatal(string(raw))
	}
	audit.identify(req, "saml", &browserauth.Identity{Subject: "subject", DisplayName: "other"}, nil)
	if audit.actor != "saml:subject" {
		t.Fatal(audit.actor)
	}
}
