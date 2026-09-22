package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeRequirements(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.toml")
	if err := os.WriteFile(path, []byte("[profiles.p]\nharness='codex'\n[profiles.p.auth]\nmode='api_key'\napi_key_env='KEY'\n"), 0600); err != nil {
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

func TestProfilePresenceValidation(t *testing.T) {
	const api = "[profiles.p]\nharness='codex'\n[profiles.p.auth]\nmode='api_key'\napi_key_env='KEY'\n"
	const account = "[credential_stores.s]\nbucket='b'\n[profiles.p]\nharness='codex'\n[profiles.p.auth]\nmode='account'\nstore='s'\nkey='auth'\n"
	for name, source := range map[string]string{
		"api store": api + "store=''", "api key": api + "key=''",
		"account api env": account + "api_key_env=''",
		"empty type":      strings.Replace(account, "bucket='b'", "bucket='b'\ntype=''", 1),
		"empty region":    strings.Replace(account, "bucket='b'", "bucket='b'\nregion=''", 1),
		"missing key":     strings.Replace(account, "key='auth'", "", 1),
		"unknown field":   api + "unknown=true",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadProfiles(strings.NewReader(source)); err == nil {
				t.Fatal("accepted invalid profile")
			}
		})
	}
	for _, source := range []string{api, account} {
		if _, err := ReadProfiles(strings.NewReader(source)); err != nil {
			t.Fatal(err)
		}
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
