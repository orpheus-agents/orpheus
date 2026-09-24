package browserauth

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crewjam/saml"

	"github.com/orpheus-agents/orpheus/internal/config"
)

func TestReturnPath(t *testing.T) {
	for _, path := range []string{"https://evil.test", "//evil.test", "/\\evil", "/%2f/evil", "/%252f/evil", "/auth/login", "/x/../auth/login", "/saml/metadata", "/\nfoo", "/foo#bar", "/sessions?q=a\nb", "/sessions?q=a%0Ab", "/sessions?q=%7f", "/%252fauth/login", "/%2e%2e/auth/login"} {
		if _, err := returnPath(path); err == nil {
			t.Errorf("accepted %q", path)
		}
	}
	for _, path := range []string{"/", "/sessions/123", "/api/v1/sessions?limit=50", "/sessions?q=a%20b", "/sessions?q=a b", "/sessions?q=50%25", "/sessions?q=https%3A%2F%2Fexample.test", ""} {
		if _, err := returnPath(path); err != nil {
			t.Errorf("rejected %q: %v", path, err)
		}
	}
}
func TestNonSAMLStateAndRoutes(t *testing.T) {
	for _, mode := range []string{"api_only", "anonymous"} {
		t.Run(mode, func(t *testing.T) {
			c := config.DefaultSettings().BrowserAuth
			c.Mode = mode
			c.PublicURL = "http://localhost:8000"
			s, err := New(c, nil)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil)
			r.AddCookie(&http.Cookie{Name: CookieName, Value: "invalid"})
			r.Header.Set("Authorization", "invalid")
			w := httptest.NewRecorder()
			state, err := s.State(w, r)
			if err != nil || state.Authenticated || state.ReadAccess != (mode == "anonymous") || state.User != nil || state.ExpiresAt != nil {
				t.Fatal(state, err)
			}
			if w.Result().Cookies()[0].MaxAge != -1 {
				t.Fatal("cookie was not cleared")
			}
			if err := s.Login(w, r); err == nil {
				t.Fatal("login enabled")
			}
			if err := s.Metadata(w, r); err == nil {
				t.Fatal("metadata enabled")
			}
			if err := s.Callback(w, r); err == nil {
				t.Fatal("callback enabled")
			}
			r.Header.Set("Origin", c.PublicURL)
			r.Header.Set("X-Orpheus-CSRF", "1")
			if err := s.Logout(httptest.NewRecorder(), r); err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Origin", "http://evil.test")
			if err := s.Logout(w, r); err == nil {
				t.Fatal("foreign origin")
			}
		})
	}
}
func TestCookies(t *testing.T) {
	token := randomToken()
	if !validToken(token) || validToken(token+"=") {
		t.Fatal("token format")
	}
	w := httptest.NewRecorder()
	cookie(w, CookieName, token, time.Now().Add(time.Hour), http.SameSiteLaxMode)
	c := w.Result().Cookies()[0]
	if !c.Secure || !c.HttpOnly || c.Path != "/" || c.Domain != "" || c.SameSite != http.SameSiteLaxMode {
		t.Fatal(c)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(c)
	r.AddCookie(c)
	if requestHash(r, CookieName) != nil {
		t.Fatal("duplicate cookie")
	}
}

func TestSAMLDiagnosticCategoriesDoNotExposePayload(t *testing.T) {
	secret := "fixture-subject secret-token <Assertion>private</Assertion>"
	for message, want := range map[string]string{
		"assertion Conditions AudienceRestriction " + secret: "audience",
		"response Issuer mismatch " + secret:                 "issuer",
		"signature validation failed " + secret:              "signature",
		"cannot parse base64: " + secret:                     "encoding",
		"`Destination` does not match requested URL or AcsURL (destination \"https://audience.test/issuer\")": "destination",
		"cannot unmarshal response: unknown element audience " + secret:                                       "xml",
		"unrecognized error quoting audience signature expired " + secret:                                     "invalid_response",
		secret: "invalid_response",
	} {
		got := samlFailureReason(&saml.InvalidResponseError{PrivateErr: errors.New(message), Response: secret})
		if got != want || strings.Contains(got, "secret") {
			t.Fatal(got, want)
		}
	}
}

func TestLogoutUsesCanonicalOrigin(t *testing.T) {
	c := config.DefaultSettings().BrowserAuth
	c.Mode = "anonymous"
	c.PublicURL = "https://HOST:443"
	s, err := New(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		origin  string
		allowed bool
	}{
		{"https://host", true}, {"http://host", false}, {"https://host:8443", false}, {"https://evil.test", false},
	} {
		r := httptest.NewRequest("POST", "/auth/logout", nil)
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("X-Orpheus-CSRF", "1")
		err := s.Logout(httptest.NewRecorder(), r)
		if (err == nil) != tc.allowed {
			t.Fatal(tc, err)
		}
	}
}

func TestSAMLDiagnosticIDPStatus(t *testing.T) {
	for _, status := range []string{"urn:oasis:names:tc:SAML:2.0:status:Responder", "private-subject-token"} {
		err := &saml.InvalidResponseError{PrivateErr: fmt.Errorf("wrapped: %w", saml.ErrBadStatus{Status: status})}
		if got := samlFailureReason(err); got != "idp_status" {
			t.Fatal(got)
		}
	}
}
