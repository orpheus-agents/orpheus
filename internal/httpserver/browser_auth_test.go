//go:build integration

package httpserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/session"

	"github.com/orpheus-agents/orpheus/client"
	"github.com/orpheus-agents/orpheus/internal/browserauth"
	"github.com/orpheus-agents/orpheus/internal/store/db"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

func TestBrowserAccessMatrix(t *testing.T) {
	_, storage := testServer(t)
	fixture := testutil.NewSAML(t, "https://orpheus.example.test")
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	hash := sha256.Sum256([]byte(token))
	if err := db.New(storage.Pool).InsertBrowserSession(t.Context(), db.InsertBrowserSessionParams{TokenHash: hash[:], Subject: "fixture", DisplayName: "operator", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"api_only", "anonymous", "saml"} {
		t.Run(mode, func(t *testing.T) {
			storage.Settings.BrowserAuth = fixture.Config
			storage.Settings.BrowserAuth.Mode = mode
			handler, err := Handler(storage, t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name, method, path, auth, cookie string
				hasAuth                          bool
				want                             int
			}{
				{name: "public state ignores bearer", method: "GET", path: "/api/v1/auth/session", auth: "broken", hasAuth: true, want: 200},
				{name: "bearer write with cookie", method: "POST", path: "/api/v1/sessions", auth: "Bearer key", hasAuth: true, cookie: token, want: 422},
				{name: "bearer read", method: "GET", path: "/api/v1/sessions", auth: "Bearer key", hasAuth: true, want: 200},
				{name: "bearer analytics", method: "GET", path: "/api/v1/analytics/overview", auth: "Bearer key", hasAuth: true, want: 200},
				{name: "bad bearer cookie", method: "GET", path: "/api/v1/sessions", auth: "Bearer bad", hasAuth: true, cookie: token, want: 401},
				{name: "empty header cookie", method: "GET", path: "/api/v1/sessions", hasAuth: true, cookie: token, want: 401},
				{name: "absent", method: "GET", path: "/api/v1/sessions", want: map[string]int{"api_only": 401, "saml": 401, "anonymous": 200}[mode]},
				{name: "cookie", method: "GET", path: "/api/v1/runs", cookie: token, want: map[string]int{"api_only": 401, "saml": 200, "anonymous": 200}[mode]},
				{name: "cookie analytics", method: "GET", path: "/api/v1/analytics/overview", cookie: token, want: map[string]int{"api_only": 401, "saml": 200, "anonymous": 200}[mode]},
				{name: "unknown read", method: "GET", path: "/api/v1/future", cookie: token, want: map[string]int{"api_only": 401, "saml": 403, "anonymous": 401}[mode]},
				{name: "write cookie", method: "POST", path: "/api/v1/sessions", cookie: token, want: map[string]int{"api_only": 401, "saml": 403, "anonymous": 401}[mode]},
				{name: "write absent", method: "POST", path: "/api/v1/sessions", want: 401},
				{name: "invalid cookie", method: "GET", path: "/api/v1/sessions", cookie: "invalid", want: map[string]int{"api_only": 401, "saml": 401, "anonymous": 200}[mode]},
				{name: "auth method", method: "PUT", path: "/auth/login", want: 405},
				{name: "metadata", method: "GET", path: "/saml/metadata", want: map[string]int{"api_only": 404, "saml": 200, "anonymous": 404}[mode]},
			} {
				t.Run(tc.name, func(t *testing.T) {
					r := httptest.NewRequest(tc.method, tc.path, nil)
					if tc.hasAuth {
						r.Header.Set("Authorization", tc.auth)
					}
					if tc.cookie != "" {
						r.AddCookie(&http.Cookie{Name: browserauth.CookieName, Value: tc.cookie})
					}
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, r)
					if w.Code != tc.want {
						t.Fatalf("got %d want %d: %s", w.Code, tc.want, w.Body.String())
					}
					if w.Header().Get("Cache-Control") != "no-store" {
						t.Fatal(w.Header())
					}
				})
			}
		})
	}
}
func TestSAMLThroughHTTPHandler(t *testing.T) {
	_, storage := testServer(t)
	fixture := testutil.NewSAML(t, "https://orpheus.example.test")
	storage.Settings.BrowserAuth = fixture.Config
	handler, err := Handler(storage, t.Context())
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/auth/login?next=/api/v1/sessions", nil)
	r.Header.Set("Host", "evil.test")
	r.Header.Set("Forwarded", "host=evil.test;proto=http")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 302 {
		t.Fatal(w.Code, w.Body.String())
	}
	target, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	relay := target.Query().Get("RelayState")
	var requestID string
	if err := storage.Pool.QueryRow(t.Context(), "SELECT request_id FROM browser_login_requests WHERE id=$1", relay).Scan(&requestID); err != nil {
		t.Fatal(err)
	}
	nonce := w.Result().Cookies()[0]
	form := url.Values{"RelayState": {relay}, "SAMLResponse": {fixture.Response(t, requestID, nil)}}.Encode()
	r = httptest.NewRequest("POST", "/auth/callback", strings.NewReader(form))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(nonce)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	var credential *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == browserauth.CookieName {
			credential = c
		}
	}
	if credential == nil {
		t.Fatal("cookie missing")
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	apiClient, err := client.NewClientWithResponses(server.URL, client.WithRequestEditorFn(func(_ context.Context, r *http.Request) error { r.AddCookie(credential); return nil }))
	if err != nil {
		t.Fatal(err)
	}
	state, err := apiClient.AuthSessionWithResponse(t.Context())
	if err != nil || state.JSON200 == nil || !state.JSON200.Authenticated {
		t.Fatal(state, err)
	}
	page, err := apiClient.ListSessionsWithResponse(t.Context(), nil)
	if err != nil || page.JSON200 == nil {
		t.Fatal(page, err)
	}
	r = httptest.NewRequest("POST", "/auth/callback", strings.NewReader(form))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(nonce)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal("replay", w.Code)
	}
	r = httptest.NewRequest("POST", "/auth/callback", strings.NewReader("SAMLResponse="+strings.Repeat("a", browserauth.CallbackLimit)))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 413 {
		t.Fatal("body limit", w.Code, w.Body.String())
	}
	// Outage must not look like logged-out state.
	storage.Pool.Close()
	r = httptest.NewRequest("GET", "/api/v1/auth/session", nil)
	r.AddCookie(credential)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestBrowserSessionStreamLifetime(t *testing.T) {
	for _, kind := range []string{"expiry", "revocation", "storage failure"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			_, storage := testServer(t)
			f := testutil.NewSAML(t, "https://orpheus.example.test")
			auth, err := browserauth.New(f.Config, storage.Pool)
			if err != nil {
				t.Fatal(err)
			}
			expiry := time.Now().Add(time.Hour)
			if kind == "expiry" {
				expiry = time.Now().Add(100 * time.Millisecond)
			}
			hash := sha256.Sum256([]byte(kind))
			if err := db.New(storage.Pool).InsertBrowserSession(t.Context(), db.InsertBrowserSessionParams{TokenHash: hash[:], Subject: "fixture", DisplayName: "operator", ExpiresAt: expiry}); err != nil {
				t.Fatal(err)
			}
			ctx, stop := watchBrowserSession(t.Context(), auth, &browserauth.Identity{TokenHash: hash[:], ExpiresAt: expiry}, 10*time.Millisecond)
			defer stop()
			if kind == "revocation" {
				if err := db.New(storage.Pool).DeleteBrowserSession(t.Context(), hash[:]); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "storage failure" {
				storage.Pool.Close()
			}
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
				t.Fatal("browser stream authorization not cancelled")
			}
		})
	}
}
func TestAnonymousWithoutServiceKeys(t *testing.T) {
	_, storage := testServer(t)
	storage.Settings.BrowserAuth.Mode = "anonymous"
	storage.Settings.PublicAPIKeys = nil
	handler, err := Handler(storage, t.Context())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	res, err := server.Client().Get(server.URL + "/api/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || !json.Valid(raw) {
		t.Fatal(res.StatusCode, string(raw))
	}
}

func TestCookieSSEClosesAtExpiry(t *testing.T) {
	original, storage := testServer(t)
	_, _, raw := requestHTTP(t, original, "POST", "/api/v1/sessions", validBody, "key", uuid.NewString())
	var accepted session.Acceptance
	if err := json.Unmarshal(raw, &accepted); err != nil {
		t.Fatal(err)
	}
	f := testutil.NewSAML(t, "https://orpheus.example.test")
	storage.Settings.BrowserAuth = f.Config
	handler, err := Handler(storage, t.Context())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	hash := sha256.Sum256([]byte(token))
	if err := db.New(storage.Pool).InsertBrowserSession(t.Context(), db.InsertBrowserSessionParams{TokenHash: hash[:], Subject: "fixture", DisplayName: "operator", ExpiresAt: time.Now().Add(2 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	page, err := storage.Events(t.Context(), accepted.SessionID, "0", 100)
	if err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequestWithContext(t.Context(), "GET", server.URL+"/api/v1/sessions/"+accepted.SessionID.String()+"/events/stream?after="+page.NextCursor, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.AddCookie(&http.Cookie{Name: browserauth.CookieName, Value: token})
	httpClient := &http.Client{Timeout: 5 * time.Second}
	response, err := httpClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != 200 {
		t.Fatal(response.StatusCode)
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal(response.Header)
	}
	body, err := io.ReadAll(response.Body)
	// Expiry also interrupts blocked writes; the connection may close before
	// net/http writes the final chunk. SSE clients reconnect after either EOF.
	if (err != nil && !errors.Is(err, io.ErrUnexpectedEOF)) || !strings.Contains(string(body), ": keep-alive") {
		t.Fatal(string(body), err)
	}
}

func TestExpiredIDPMetadataKeepsBearerAndCookieAPIAvailable(t *testing.T) {
	_, storage := testServer(t)
	f := testutil.NewSAML(t, "https://orpheus.example.test")
	raw, err := os.ReadFile(f.Config.MetadataFile)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), "entityID=", `validUntil="2000-01-01T00:00:00Z" entityID=`, 1))
	if err := os.WriteFile(f.Config.MetadataFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	storage.Settings.BrowserAuth = f.Config
	handler, err := Handler(storage, t.Context())
	if err != nil {
		t.Fatal("expired metadata prevents serve", err)
	}
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	hash := sha256.Sum256([]byte(token))
	if err := db.New(storage.Pool).InsertBrowserSession(t.Context(), db.InsertBrowserSessionParams{TokenHash: hash[:], Subject: "existing", DisplayName: "operator", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	for _, bearer := range []bool{true, false} {
		r := httptest.NewRequest("GET", "/api/v1/sessions", nil)
		if bearer {
			r.Header.Set("Authorization", "Bearer key")
		} else {
			r.AddCookie(&http.Cookie{Name: browserauth.CookieName, Value: token})
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal("API unavailable", w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/auth/login", nil))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
}
