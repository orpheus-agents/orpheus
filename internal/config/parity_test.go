package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeRequirements(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.toml")
	if err := os.WriteFile(path, []byte("# deployment without profiles\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s := DefaultSettings()
	s.ConfigFile = path
	s.EnvEncryptionKey = base64.URLEncoding.EncodeToString(make([]byte, 32))
	if _, _, err := s.Runtime(true); err == nil {
		t.Fatal("API accepted missing public keys")
	}
	if _, _, err := s.Runtime(false); err != nil {
		t.Fatal(err)
	}
	s.BrowserAuth.Mode = "anonymous"
	if _, _, err := s.Runtime(true); err != nil {
		t.Fatal("anonymous requires service keys", err)
	}
	s.BrowserAuth.Mode = "saml"
	if _, _, err := s.Runtime(true); err == nil {
		t.Fatal("serve accepted missing SAML configuration")
	}
	if _, _, err := s.Runtime(false); err != nil {
		t.Fatal("worker requires SAML configuration", err)
	}
	s.BrowserAuth.Mode = "api_only"
	s.PublicAPIKeys = []string{"key"}
	if _, _, err := s.Runtime(true); err != nil {
		t.Fatal(err)
	}
	s.EnvEncryptionKey = "invalid"
	if _, _, err := s.Runtime(false); err == nil {
		t.Fatal("accepted invalid cipher key")
	}
	s.ConfigFile = path + "-missing"
	if _, _, err := s.Runtime(false); err == nil {
		t.Fatal("accepted missing profile file")
	}
}

func TestSettingsRejectNullListsAndElements(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://fixture")
	for _, name := range []string{"PUBLIC_API_KEYS", "HARNESS_ENV_ALLOWLIST"} {
		for _, value := range []string{"null", "[null]", "[\"a\",null]"} {
			t.Run(name+value, func(t *testing.T) {
				t.Setenv(name, value)
				if _, err := Load(); err == nil {
					t.Fatal("accepted null string/list")
				}
			})
		}
	}
}
