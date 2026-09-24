package config

import (
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus/internal/session"
)

func TestBrowserSettings(t *testing.T) {
	base := DefaultSettings().BrowserAuth
	if base.Mode != "api_only" {
		t.Fatal(base)
	}
	for _, edit := range []func(*BrowserAuth){
		func(c *BrowserAuth) { c.Mode = "none" }, func(c *BrowserAuth) { c.Mode = "saml" },
		func(c *BrowserAuth) { c.PublicURL = "https://example.test/" }, func(c *BrowserAuth) { c.PublicURL = "https://user:pass@example.test" },
		func(c *BrowserAuth) { c.PublicURL = "https://example.test?" }, func(c *BrowserAuth) { c.PublicURL = "https://example.test#" },
		func(c *BrowserAuth) { c.ttlSeconds = "1" }, func(c *BrowserAuth) { c.ttlSeconds = "99999999999999999999" }, func(c *BrowserAuth) { c.ttlSeconds = "invalid" },
	} {
		c := base
		edit(&c)
		if _, err := c.Validated(); err == nil {
			t.Fatal("accepted", c)
		}
	}
	t.Setenv("DATABASE_URL", "postgres://unused")
	t.Setenv("ORPHEUS_BROWSER_AUTH", "anonymous")
	t.Setenv("BROWSER_SESSION_TTL_SECONDS", "600")
	settings, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c, err := settings.BrowserAuth.Validated()
	if err != nil || c.Mode != "anonymous" || c.SessionTTL != 10*time.Minute {
		t.Fatal(c, err)
	}
	for _, name := range []string{"SAML_SP_KEY_FILE", "SAML_IDP_METADATA_FILE", "SAML_SP_CERT_FILE", "SAML_SP_ENTITY_ID", "BROWSER_SESSION_TTL_SECONDS", "ORPHEUS_BROWSER_AUTH", "ORPHEUS_PUBLIC_URL"} {
		if err := ValidateRunEnvironment(map[string]string{name: "secret"}, nil, nil); err == nil {
			t.Fatal(name)
		}
		if err := ValidateSandbox(session.SandboxInput{Template: "test", EnvFrom: []string{name}}); err == nil {
			t.Fatal(name)
		}
	}
}

func TestBrowserPublicOriginCanonicalization(t *testing.T) {
	for input, want := range map[string]string{
		"https://HOST:443": "https://host", "http://LOCALHOST:80": "http://localhost",
		"https://HOST:8443": "https://host:8443", "http://[::1]:80": "http://[::1]",
		"https://[::1]:8443": "https://[::1]:8443",
	} {
		c := DefaultSettings().BrowserAuth
		c.PublicURL = input
		c, err := c.Validated()
		if err != nil || c.PublicURL != want {
			t.Fatal(input, c.PublicURL, err)
		}
	}
}
