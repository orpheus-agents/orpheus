//go:build integration

package browserauth

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beevik/etree"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store/db"
	"github.com/orpheus-agents/orpheus/internal/testutil"
)

type loginFixture struct {
	relay, id string
	nonce     *http.Cookie
}

func beginLogin(t *testing.T, s *Service, pool *pgxpool.Pool, cookies ...*http.Cookie) loginFixture {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/auth/login?next=/api/v1/sessions", nil).WithContext(t.Context())
	for _, c := range cookies {
		r.AddCookie(c)
	}
	if err := s.Login(w, r); err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	relay := target.Query().Get("RelayState")
	if len(target.Query().Get("Signature")) == 0 || !strings.Contains(target.Query().Get("SigAlg"), "sha256") {
		t.Fatal("unsigned/weak AuthnRequest")
	}
	var id string
	if err := pool.QueryRow(t.Context(), "SELECT request_id FROM browser_login_requests WHERE id=$1", relay).Scan(&id); err != nil {
		t.Fatal(err)
	}
	nonce := w.Result().Cookies()[0]
	if nonce.SameSite != http.SameSiteNoneMode || !nonce.Secure || !nonce.HttpOnly {
		t.Fatal(nonce)
	}
	return loginFixture{relay: relay, id: id, nonce: nonce}
}
func callbackRequest(t *testing.T, login loginFixture, response string) *http.Request {
	t.Helper()
	r := httptest.NewRequest("POST", "/auth/callback", strings.NewReader(url.Values{"RelayState": {login.relay}, "SAMLResponse": {response}}.Encode())).WithContext(t.Context())
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(login.nonce)
	return r
}
func TestSAMLTransactionReplayAndReplica(t *testing.T) {
	pool := testutil.Database(t)
	f := testutil.NewSAML(t, "https://orpheus.example.test")
	first, err := New(f.Config, pool)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(f.Config, pool)
	if err != nil {
		t.Fatal(err)
	}
	login := beginLogin(t, first, pool)
	other := beginLogin(t, first, pool)
	response := f.Response(t, login.id, nil)
	// The assertion for a different tab cannot consume this request.
	if err := second.Callback(httptest.NewRecorder(), callbackRequest(t, other, response)); err == nil {
		t.Fatal("cross-tab response accepted")
	}
	wrong := login
	wrong.nonce = other.nonce
	if err := second.Callback(httptest.NewRecorder(), callbackRequest(t, wrong, response)); err == nil {
		t.Fatal("wrong browser accepted")
	}
	var wg sync.WaitGroup
	results := make(chan *httptest.ResponseRecorder, 2)
	failures := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			w := httptest.NewRecorder()
			if err := second.Callback(w, callbackRequest(t, login, response)); err != nil {
				failures <- err
			} else {
				results <- w
			}
		})
	}
	wg.Wait()
	close(results)
	close(failures)
	if len(results) != 1 || len(failures) != 1 {
		t.Fatalf("success=%d failures=%d", len(results), len(failures))
	}
	w := <-results
	if w.Code != 303 || w.Header().Get("Location") != "/api/v1/sessions" {
		t.Fatal(w)
	}
	var credential *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == CookieName {
			credential = c
		}
	}
	if credential == nil {
		t.Fatal("no session cookie")
	}
	r := httptest.NewRequest("GET", "/api/v1/auth/session", nil).WithContext(t.Context())
	r.AddCookie(credential)
	state, err := first.State(httptest.NewRecorder(), r)
	if err != nil || !state.Authenticated || state.User.DisplayName != "operator" || time.Until(*state.ExpiresAt) > time.Hour {
		t.Fatal(state, err)
	}
	var sessions, pending int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM browser_sessions").Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM browser_login_requests").Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 || pending != 1 {
		t.Fatal(sessions, pending)
	}
	// Replace the old browser session atomically on a subsequent login.
	replacement := beginLogin(t, second, pool, credential)
	req := callbackRequest(t, replacement, f.Response(t, replacement.id, nil))
	// Cross-site IdP POST omits the Lax session cookie; only the nonce survives.
	w = httptest.NewRecorder()
	if err := first.Callback(w, req); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Authenticate(r); !errors.Is(err, ErrNoSession) {
		t.Fatal("old session", err)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == CookieName {
			credential = c
		}
	}
	logout := httptest.NewRequest("POST", "/auth/logout", nil).WithContext(t.Context())
	logout.AddCookie(credential)
	for _, origin := range []string{"", "https://evil.test"} {
		logout.Header.Set("Origin", origin)
		if err := second.Logout(httptest.NewRecorder(), logout); err == nil {
			t.Fatal("bad origin")
		}
	}
	logout.Header.Set("Origin", f.Config.PublicURL)
	logout.Header.Set("X-Orpheus-CSRF", "1")
	if err := second.Logout(httptest.NewRecorder(), logout); err != nil {
		t.Fatal(err)
	}
	if err := second.Logout(httptest.NewRecorder(), logout); err != nil {
		t.Fatal("not idempotent", err)
	}
	if _, err := first.Authenticate(logout); !errors.Is(err, ErrNoSession) {
		t.Fatal(err)
	}
}
func TestSAMLInvalidAssertions(t *testing.T) {
	pool := testutil.Database(t)
	f := testutil.NewSAML(t, "https://orpheus.example.test")
	s, err := New(f.Config, pool)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*etree.Element){
		"audience": func(e *etree.Element) { e.FindElement(".//saml:Audience").SetText("wrong") },
		"missing audience": func(e *etree.Element) {
			e.FindElement(".//saml:Conditions").RemoveChild(e.FindElement(".//saml:AudienceRestriction"))
		},
		"issuer": func(e *etree.Element) { e.FindElement(".//saml:Assertion/saml:Issuer").SetText("wrong") },
		"recipient": func(e *etree.Element) {
			e.FindElement(".//saml:SubjectConfirmationData").CreateAttr("Recipient", "https://evil.test")
		},
		"destination": func(e *etree.Element) { e.CreateAttr("Destination", "https://evil.test") },
		"request":     func(e *etree.Element) { e.CreateAttr("InResponseTo", "wrong") },
		"expired": func(e *etree.Element) {
			e.FindElement(".//saml:Conditions").CreateAttr("NotOnOrAfter", time.Now().Add(-61*time.Second).Format(time.RFC3339Nano))
		},
		"future": func(e *etree.Element) {
			e.FindElement(".//saml:Conditions").CreateAttr("NotBefore", time.Now().Add(2*time.Minute).Format(time.RFC3339Nano))
		},
		"session expiry": func(e *etree.Element) {
			e.FindElement(".//saml:AuthnStatement").CreateAttr("SessionNotOnOrAfter", time.Now().Add(-time.Second).Format(time.RFC3339Nano))
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			login := beginLogin(t, s, pool)
			err := s.Callback(httptest.NewRecorder(), callbackRequest(t, login, f.Response(t, login.id, edit)))
			p, ok := errors.AsType[*session.APIError](err)
			if !ok || p.Status != 401 {
				t.Fatal(err)
			}
		})
	}
	login := beginLogin(t, s, pool)
	if err := s.Callback(httptest.NewRecorder(), callbackRequest(t, login, "malformed")); err == nil {
		t.Fatal("malformed accepted")
	}
	response := f.Response(t, login.id, nil)
	req := callbackRequest(t, login, response)
	req.Header.Del("Cookie")
	if err := s.Callback(httptest.NewRecorder(), req); err == nil {
		t.Fatal("missing nonce")
	}
	if _, err := pool.Exec(t.Context(), "UPDATE browser_login_requests SET expires_at=now()-interval '1 second' WHERE id=$1", login.relay); err != nil {
		t.Fatal(err)
	}
	if err := s.Callback(httptest.NewRecorder(), callbackRequest(t, login, response)); err == nil {
		t.Fatal("expired pending accepted")
	}
}
func TestBrowserExpiryCleanupAndStorageFailure(t *testing.T) {
	pool := testutil.Database(t)
	f := testutil.NewSAML(t, "https://orpheus.example.test")
	s, err := New(f.Config, pool)
	if err != nil {
		t.Fatal(err)
	}
	login := beginLogin(t, s, pool)
	w := httptest.NewRecorder()
	if err := s.Callback(w, callbackRequest(t, login, f.Response(t, login.id, nil))); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/", nil).WithContext(t.Context())
	for _, c := range w.Result().Cookies() {
		if c.Name == CookieName {
			r.AddCookie(c)
		}
	}
	if _, err := pool.Exec(t.Context(), "UPDATE browser_sessions SET expires_at=now()-interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(r); !errors.Is(err, ErrNoSession) {
		t.Fatal(err)
	}
	if n, err := db.New(pool).CleanupBrowserSessions(t.Context()); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	_ = beginLogin(t, s, pool)
	if _, err := pool.Exec(t.Context(), "UPDATE browser_login_requests SET expires_at=now()-interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	if n, err := db.New(pool).CleanupBrowserLogins(t.Context()); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	pool.Close()
	if _, err := s.State(httptest.NewRecorder(), r); err == nil || errors.Is(err, ErrNoSession) {
		t.Fatal("storage failure concealed", err)
	}
}

func TestCallbackRollsBackPendingConsumption(t *testing.T) {
	pool := testutil.Database(t)
	f := testutil.NewSAML(t, "https://orpheus.example.test")
	s, err := New(f.Config, pool)
	if err != nil {
		t.Fatal(err)
	}
	oldToken := randomToken()
	if err := db.New(pool).InsertBrowserSession(t.Context(), db.InsertBrowserSessionParams{TokenHash: tokenHash(oldToken), Subject: "old", DisplayName: "old", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	oldCookie := &http.Cookie{Name: CookieName, Value: oldToken}
	oldRequest := httptest.NewRequest("GET", "/", nil).WithContext(t.Context())
	oldRequest.AddCookie(oldCookie)
	login := beginLogin(t, s, pool, oldCookie)
	response := f.Response(t, login.id, nil)
	// Reject the insert after the one-time login has been deleted in the transaction.
	if _, err := pool.Exec(t.Context(), "ALTER TABLE browser_sessions ADD CONSTRAINT fixture_reject CHECK (false) NOT VALID"); err != nil {
		t.Fatal(err)
	}
	if err := s.Callback(httptest.NewRecorder(), callbackRequest(t, login, response)); err == nil {
		t.Fatal("insert unexpectedly succeeded")
	}
	if _, err := s.Authenticate(oldRequest); err != nil {
		t.Fatal("old session was revoked on failed insert", err)
	}
	if _, err := pool.Exec(t.Context(), "ALTER TABLE browser_sessions DROP CONSTRAINT fixture_reject"); err != nil {
		t.Fatal(err)
	}
	if err := s.Callback(httptest.NewRecorder(), callbackRequest(t, login, response)); err != nil {
		t.Fatal("pending request was consumed on failed session insert", err)
	}
}
func TestSAMLSignatureAndMetadataFailures(t *testing.T) {
	pool := testutil.Database(t)
	f := testutil.NewSAML(t, "https://orpheus.example.test")
	s, err := New(f.Config, pool)
	if err != nil {
		t.Fatal(err)
	}
	login := beginLogin(t, s, pool)
	signed := f.Response(t, login.id, nil)
	raw, err := base64.StdEncoding.DecodeString(signed)
	if err != nil {
		t.Fatal(err)
	}
	tampered := base64.StdEncoding.EncodeToString([]byte(strings.ReplaceAll(string(raw), "fixture-subject", "attacker")))
	if err := s.Callback(httptest.NewRecorder(), callbackRequest(t, login, tampered)); err == nil {
		t.Fatal("tampered signature accepted")
	}
	// Metadata that expires after startup must stop new logins, not just fail startup.
	s.sp.IDPMetadata.ValidUntil = time.Now().Add(-time.Second)
	if err := s.Login(httptest.NewRecorder(), httptest.NewRequest("GET", "/auth/login", nil)); err == nil {
		t.Fatal("expired metadata accepted")
	}
	metadata, err := os.ReadFile(f.Config.MetadataFile)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"malformed":           "not xml",
		"missing certificate": strings.ReplaceAll(string(metadata), `use="signing"`, `use="encryption"`),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(f.Config.MetadataFile, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := New(f.Config, pool); err == nil {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
}

func TestExpiredMetadataDoesNotPreventStartup(t *testing.T) {
	f := testutil.NewSAML(t, "https://orpheus.example.test")
	metadata, err := os.ReadFile(f.Config.MetadataFile)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"root":    strings.Replace(string(metadata), "entityID=", `validUntil="2000-01-01T00:00:00Z" entityID=`, 1),
		"wrapper": `<EntitiesDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" validUntil="2000-01-01T00:00:00Z">` + string(metadata) + `</EntitiesDescriptor>`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(f.Config.MetadataFile, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			s, err := New(f.Config, nil)
			if err != nil {
				t.Fatal("expired metadata blocked startup", err)
			}
			err = s.Login(httptest.NewRecorder(), httptest.NewRequest("GET", "/auth/login", nil))
			problem, ok := errors.AsType[*session.APIError](err)
			if !ok || problem.Status != 503 || problem.Problem.Code != "auth_unavailable" {
				t.Fatal(err)
			}
		})
	}
}
func TestSAMLDisplayNames(t *testing.T) {
	pool := testutil.Database(t)
	f := testutil.NewSAML(t, "https://orpheus.example.test")
	s, err := New(f.Config, pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, friendly, value, want string }{
		{"preferred_username", "", "operator", "operator"},
		{"urn:oid:1.2.840.113549.1.9.1", "email", "operator@example.test", "operator@example.test"},
		{"unknown", "", "ignored", "fixture-subject"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			login := beginLogin(t, s, pool)
			signed := f.Response(t, login.id, func(e *etree.Element) {
				a := e.FindElement(".//saml:Attribute")
				a.CreateAttr("Name", tc.name)
				a.CreateAttr("FriendlyName", tc.friendly)
				a.FindElement("saml:AttributeValue").SetText(tc.value)
			})
			w := httptest.NewRecorder()
			if err := s.Callback(w, callbackRequest(t, login, signed)); err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("GET", "/", nil).WithContext(t.Context())
			for _, c := range w.Result().Cookies() {
				if c.Name == CookieName {
					r.AddCookie(c)
				}
			}
			identity, err := s.Authenticate(r)
			if err != nil || identity.DisplayName != tc.want {
				t.Fatal(identity, err)
			}
		})
	}
}
