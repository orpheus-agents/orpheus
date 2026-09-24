//go:build integration

package httpserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/browserauth"
	"github.com/orpheus-agents/orpheus/internal/testutil"
	"golang.org/x/net/html"
)

// Keycloak runs on the isolated Compose network. A trusted test TLS proxy gives
// the IdP an HTTPS public origin; the SP runs the unmodified production Handler.
func TestKeycloakRoundTrip(t *testing.T) {
	upstream := os.Getenv("TEST_KEYCLOAK_URL")
	if upstream == "" {
		t.Skip("run make test-saml for the real Keycloak round trip")
	}
	target, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(target))
	defer proxy.Close()
	sp := httptest.NewUnstartedServer(nil)
	sp.StartTLS()
	defer sp.Close()
	roots := x509.NewCertPool()
	roots.AddCert(proxy.Certificate())
	roots.AddCert(sp.Certificate())
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Transport: transport, Jar: jar, Timeout: 15 * time.Second}
	direct := &http.Client{Timeout: 5 * time.Second}
	var adminToken string
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		res, err := direct.PostForm(upstream+"/realms/master/protocol/openid-connect/token", url.Values{"client_id": {"admin-cli"}, "grant_type": {"password"}, "username": {"fixture-admin"}, "password": {"fixture-only-password"}})
		if err == nil {
			var body struct {
				AccessToken string `json:"access_token"`
			}
			_ = json.NewDecoder(res.Body).Decode(&body)
			_ = res.Body.Close()
			adminToken = body.AccessToken
			if adminToken != "" {
				break
			}
		}
		select {
		case <-t.Context().Done():
			t.Fatal("cancelled")
		case <-time.After(time.Second):
		}
	}
	if adminToken == "" {
		t.Fatal("test Keycloak did not become ready")
	}
	realm := "orpheus-" + uuid.NewString()
	admin := func(ctx context.Context, method, path, body string, want int) {
		t.Helper()
		r, err := http.NewRequestWithContext(ctx, method, upstream+"/admin/realms"+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+adminToken)
		r.Header.Set("Content-Type", "application/json")
		res, err := direct.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		if res.StatusCode != want {
			raw, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
			t.Fatalf("Keycloak admin status %d: %s", res.StatusCode, raw)
		}
	}
	realmBody, _ := json.Marshal(map[string]any{"realm": realm, "enabled": true, "sslRequired": "none", "attributes": map[string]string{"frontendUrl": proxy.URL}})
	admin(t.Context(), "POST", "", string(realmBody), 201)
	defer admin(context.Background(), "DELETE", "/"+realm, "", 204)
	f := testutil.NewSAML(t, sp.URL)
	cert, _ := pem.Decode(mustRead(t, f.Config.CertFile))
	clientBody, _ := json.Marshal(map[string]any{
		"clientId": f.Config.EntityID, "enabled": true, "protocol": "saml", "redirectUris": []string{sp.URL + "/auth/callback"},
		"attributes": map[string]string{
			"saml.assertion.signature": "true", "saml.server.signature": "true", "saml.client.signature": "true",
			"saml.signature.algorithm": "RSA_SHA256", "saml.signing.certificate": base64.StdEncoding.EncodeToString(cert.Bytes),
			"saml.force.post.binding": "true", "saml_assertion_consumer_url_post": sp.URL + "/auth/callback",
			"saml_name_id_format": "username",
		},
	})
	admin(t.Context(), "POST", "/"+realm+"/clients", string(clientBody), 201)
	userBody := `{"username":"operator","firstName":"Test","lastName":"Operator","email":"operator@example.test","emailVerified":true,"enabled":true,"credentials":[{"type":"password","value":"fixture-password","temporary":false}]}`
	admin(t.Context(), "POST", "/"+realm+"/users", userBody, 201)
	res, err := browser.Get(proxy.URL + "/realms/" + realm + "/protocol/saml/descriptor")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err != nil || res.StatusCode != 200 {
		t.Fatal("metadata", res.StatusCode, err)
	}
	if err := os.WriteFile(f.Config.MetadataFile, metadata, 0600); err != nil {
		t.Fatal(err)
	}
	_, storage := testServer(t)
	storage.Settings.BrowserAuth = f.Config
	handler, err := Handler(storage, t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sp.Config.Handler = handler
	login := func(expectPassword bool) {
		t.Helper()
		res, err := browser.Get(sp.URL + "/auth/login?next=" + url.QueryEscape("/api/v1/sessions?namespace=a%20b"))
		if err != nil {
			t.Fatal(err)
		}
		action, fields := htmlForm(t, res)
		_ = res.Body.Close()
		_, hasSAML := fields["SAMLResponse"]
		if hasSAML == expectPassword {
			t.Fatalf("unexpected password prompt: expected %v", expectPassword)
		}
		if !hasSAML {
			fields.Set("username", "operator")
			fields.Set("password", "fixture-password")
			res, err = browser.PostForm(action, fields)
			if err != nil {
				t.Fatal(err)
			}
			action, fields = htmlForm(t, res)
			_ = res.Body.Close()
		}
		if fields.Get("SAMLResponse") == "" {
			t.Fatalf("IdP did not return SAMLResponse; form action: %s", action)
		}
		res, err = browser.PostForm(action, fields)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err != nil || res.StatusCode != 200 || res.Request.URL.Path != "/api/v1/sessions" || !json.Valid(raw) {
			t.Fatalf("callback failed: %d %s %v", res.StatusCode, raw, err)
		}
		if res.Request.URL.RawQuery != "namespace=a%20b" {
			t.Fatal("return query changed", res.Request.URL.RawQuery)
		}
		stateResponse, err := browser.Get(sp.URL + "/api/v1/auth/session")
		if err != nil {
			t.Fatal(err)
		}
		var state browserauth.State
		err = json.NewDecoder(stateResponse.Body).Decode(&state)
		_ = stateResponse.Body.Close()
		if err != nil || state.User == nil || state.User.DisplayName != "operator" {
			t.Fatalf("unexpected browser identity: %+v, %v", state, err)
		}
		var subject string
		if err := storage.Pool.QueryRow(t.Context(), "SELECT subject FROM browser_sessions ORDER BY created_at DESC LIMIT 1").Scan(&subject); err != nil || subject != "operator" {
			t.Fatal("unexpected subject", subject, err)
		}
	}
	login(true)
	// The IdP SSO session makes the next login complete without credentials.
	login(false)
	spURL, _ := url.Parse(sp.URL)
	var revoked *http.Cookie
	for _, c := range jar.Cookies(spURL) {
		if c.Name == browserauth.CookieName {
			revoked = c
		}
	}
	if revoked == nil {
		t.Fatal("no browser cookie")
	}
	logout, err := http.NewRequestWithContext(t.Context(), "POST", sp.URL+"/auth/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	logout.Header.Set("Origin", sp.URL)
	logout.Header.Set("X-Orpheus-CSRF", "1")
	res, err = browser.Do(logout)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 204 {
		t.Fatal("logout", res.StatusCode)
	}
	r, err := http.NewRequestWithContext(t.Context(), "GET", sp.URL+"/api/v1/sessions", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.AddCookie(revoked)
	withoutJar := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	res, err = withoutJar.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatal("revoked cookie", res.StatusCode)
	}
	login(false)
	t.Log("Keycloak: signed AuthnRequest/Response, JSON API, existing SSO, logout, revoked cookie and re-login passed")
}
func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func htmlForm(t *testing.T, res *http.Response) (string, url.Values) {
	t.Helper()
	defer func() { _ = res.Body.Close() }()
	root, err := html.Parse(io.LimitReader(res.Body, 2<<20))
	if err != nil {
		t.Fatal(err)
	}
	fields := url.Values{}
	action := ""
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		attrs := map[string]string{}
		for _, a := range n.Attr {
			attrs[a.Key] = a.Val
		}
		if n.Type == html.ElementNode && n.Data == "form" && action == "" {
			action = attrs["action"]
		}
		if n.Type == html.ElementNode && n.Data == "input" && attrs["name"] != "" {
			fields.Add(attrs["name"], attrs["value"])
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(root)
	if action == "" {
		t.Fatalf("no form from %s (status %d)", res.Request.URL.Path, res.StatusCode)
	}
	parsed, err := url.Parse(action)
	if err != nil {
		t.Fatal(err)
	}
	return res.Request.URL.ResolveReference(parsed).String(), fields
}
