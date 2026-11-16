// Package llmlegacy reads the immutable RFC039 migration input. Runtime must not import it.
package llmlegacy

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/pelletier/go-toml/v2"
)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var accountID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

type Auth struct {
	Mode      string `toml:"mode"`
	AccountID string `toml:"account_id"`
	APIKeyEnv string `toml:"api_key_env"`
	Store     string `toml:"store"`
	Key       string `toml:"key"`
}
type CodexProfile struct {
	Effort      string `toml:"effort"`
	Summary     string `toml:"summary"`
	Personality string `toml:"personality"`
	ServiceTier string `toml:"service_tier"`
}
type Profile struct {
	Description  *string      `toml:"description"`
	Harness      string       `toml:"harness"`
	Model        *string      `toml:"model"`
	Codex        CodexProfile `toml:"codex"`
	Instructions string       `toml:"instructions"`
	Auth         Auth         `toml:"auth"`
}
type Profiles struct {
	Services         map[string]any                     `toml:"services"`
	Profiles         map[string]Profile                 `toml:"profiles"`
	CredentialStores map[string]session.CredentialStore `toml:"credential_stores"`
}

func ReadProfiles(r io.Reader) (Profiles, error) {
	// Presence matters: an omitted default and an explicitly empty value are
	// different, as are forbidden fields in either authentication variant.
	var source struct {
		Services map[string]any `toml:"services"`
		Profiles map[string]struct {
			Description *string `toml:"description"`
			Harness     string  `toml:"harness"`
			Model       *string `toml:"model"`
			Codex       struct {
				Effort      *string `toml:"effort"`
				Summary     *string `toml:"summary"`
				Personality *string `toml:"personality"`
				ServiceTier *string `toml:"service_tier"`
			} `toml:"codex"`
			Instructions string `toml:"instructions"`
			Auth         struct {
				Mode      string  `toml:"mode"`
				AccountID *string `toml:"account_id"`
				APIKeyEnv *string `toml:"api_key_env"`
				Store     *string `toml:"store"`
				Key       *string `toml:"key"`
			} `toml:"auth"`
		} `toml:"profiles"`
		CredentialStores map[string]struct {
			Type        *string `toml:"type"`
			Bucket      string  `toml:"bucket"`
			Region      *string `toml:"region"`
			EndpointURL *string `toml:"endpoint_url"`
		} `toml:"credential_stores"`
	}
	p := Profiles{Profiles: map[string]Profile{}, CredentialStores: map[string]session.CredentialStore{}}
	if err := toml.NewDecoder(r).DisallowUnknownFields().Decode(&source); err != nil {
		return p, err
	}
	if len(source.Profiles) == 0 {
		return p, errors.New("at least one named profile is required")
	}

	p.Services = source.Services
	for name, raw := range source.CredentialStores {
		s := session.CredentialStore{Type: "s3", Bucket: raw.Bucket, Region: "us-east-1", EndpointURL: raw.EndpointURL}
		if raw.Type != nil {
			s.Type = *raw.Type
		}
		if raw.Region != nil {
			s.Region = *raw.Region
		}
		if s.Type != "s3" || s.Bucket == "" || s.Region == "" || (s.EndpointURL != nil && *s.EndpointURL == "") {
			return p, errors.New("invalid credential store")
		}
		p.CredentialStores[name] = s
	}
	for name, raw := range source.Profiles {
		v := Profile{Description: normalizedDescription(raw.Description), Harness: raw.Harness, Model: raw.Model, Instructions: raw.Instructions, Auth: Auth{Mode: raw.Auth.Mode}}
		for _, option := range []struct {
			name   string
			value  *string
			target *string
		}{
			{"effort", raw.Codex.Effort, &v.Codex.Effort},
			{"summary", raw.Codex.Summary, &v.Codex.Summary},
			{"personality", raw.Codex.Personality, &v.Codex.Personality},
			{"service_tier", raw.Codex.ServiceTier, &v.Codex.ServiceTier},
		} {
			if option.value != nil {
				if !validCodexOption(option.name, *option.value) {
					return p, fmt.Errorf("invalid profile %q: codex.%s", name, option.name)
				}
				*option.target = *option.value
			}
		}
		if strings.TrimSpace(name) == "" || v.Harness != "codex" || (v.Model != nil && *v.Model == "") {
			return p, errors.New("invalid profile")
		}
		switch v.Auth.Mode {
		case "api_key":
			if raw.Auth.APIKeyEnv == nil || !envName.MatchString(*raw.Auth.APIKeyEnv) || raw.Auth.Store != nil || raw.Auth.Key != nil || raw.Auth.AccountID != nil {
				return p, errors.New("invalid API key profile")
			}
			v.Auth.APIKeyEnv = *raw.Auth.APIKeyEnv
		case "account":
			if raw.Auth.Store == nil || *raw.Auth.Store == "" || raw.Auth.Key == nil || *raw.Auth.Key == "" || raw.Auth.APIKeyEnv != nil || raw.Auth.AccountID == nil || len(*raw.Auth.AccountID) > 128 || !accountID.MatchString(*raw.Auth.AccountID) {
				return p, errors.New("invalid account profile")
			}
			v.Auth.Store, v.Auth.Key, v.Auth.AccountID = *raw.Auth.Store, *raw.Auth.Key, *raw.Auth.AccountID
			if _, ok := p.CredentialStores[v.Auth.Store]; !ok {
				return p, errors.New("invalid account profile")
			}
		default:
			return p, errors.New("invalid authentication mode")
		}
		p.Profiles[name] = v
	}
	byID, bySource := map[string]string{}, map[string]string{}
	for _, v := range p.Profiles {
		if v.Auth.Mode != "account" {
			continue
		}
		credentials := session.Credentials{Mode: "account", AccountID: v.Auth.AccountID, Store: new(p.CredentialStores[v.Auth.Store]), Key: v.Auth.Key}
		fingerprint, err := config.SourceFingerprint(v.Harness, credentials)
		if err != nil {
			return p, err
		}
		if old, ok := byID[v.Auth.AccountID]; ok && old != fingerprint {
			return p, errors.New("account_id refers to multiple credential sources")
		}
		if old, ok := bySource[fingerprint]; ok && old != v.Auth.AccountID {
			return p, errors.New("credential source refers to multiple account_id values")
		}
		byID[v.Auth.AccountID], bySource[fingerprint] = fingerprint, v.Auth.AccountID
	}
	if len(byID) > 1000 {
		return p, errors.New("too many account_id values")
	}
	return p, nil
}

func normalizedDescription(value *string) *string {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	return value
}

var codexOptionEnums = map[string][]string{
	"effort":      {"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"},
	"summary":     {"auto", "concise", "detailed", "none"},
	"personality": {"none", "friendly", "pragmatic"},
}

func validCodexOption(name, value string) bool {
	if name == "service_tier" {
		// The app-server protocol accepts a string; supported tiers depend on the provider.
		return value != "" && strings.TrimSpace(value) == value
	}
	return slices.Contains(codexOptionEnums[name], value)
}
func LoadProfiles(path string) (Profiles, error) {
	f, err := os.Open(path)
	if err != nil {
		return Profiles{}, err
	}
	defer func() { _ = f.Close() }()
	return ReadProfiles(f)
}
