package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/orpheus-agents/orpheus/internal/session"
)

// Account is a configured credential identity. Fingerprint is never exposed by HTTP.
type Account struct {
	ID          string
	Profiles    []string
	Fingerprint string
}

func (p Profiles) Accounts() []Account {
	byID := map[string]*Account{}
	for name, profile := range p.Profiles {
		if profile.Auth.Mode != "account" {
			continue
		}
		account := byID[profile.Auth.AccountID]
		if account == nil {
			credentials := session.Credentials{Mode: "account", AccountID: profile.Auth.AccountID, Store: new(p.CredentialStores[profile.Auth.Store]), Key: profile.Auth.Key}
			fingerprint, _ := SourceFingerprint(profile.Harness, credentials) // ReadProfiles validated the source.
			account = &Account{ID: profile.Auth.AccountID, Fingerprint: fingerprint}
			byID[account.ID] = account
		}
		account.Profiles = append(account.Profiles, name)
	}
	accounts := make([]Account, 0, len(byID))
	for _, account := range byID {
		slices.Sort(account.Profiles)
		accounts = append(accounts, *account)
	}
	slices.SortFunc(accounts, func(a, b Account) int { return strings.Compare(a.ID, b.ID) })
	return accounts
}

// SourceFingerprint binds an immutable session snapshot to the current profile
// catalogue without exposing credential locations or provider identities.
func SourceFingerprint(harness string, credentials session.Credentials) (string, error) {
	if credentials.Mode != "account" || credentials.Store == nil || harness == "" {
		return "", errors.New("invalid account credential source")
	}
	endpoint, err := canonicalEndpoint(credentials.Store.EndpointURL)
	if err != nil {
		return "", err
	}
	canonical := struct {
		Harness  string  `json:"harness"`
		Mode     string  `json:"mode"`
		Type     string  `json:"store_type"`
		Endpoint *string `json:"endpoint_url"`
		Region   string  `json:"region"`
		Bucket   string  `json:"bucket"`
		Key      string  `json:"key"`
	}{harness, credentials.Mode, credentials.Store.Type, endpoint, credentials.Store.Region, credentials.Store.Bucket, credentials.Key}
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func canonicalEndpoint(raw *string) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	u, err := url.Parse(*raw)
	if err != nil {
		return nil, errors.New("invalid credential store endpoint")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("invalid credential store endpoint")
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("invalid credential store endpoint port")
		}
		port = strconv.Itoa(n)
	}
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	switch {
	case port != "":
		u.Host = net.JoinHostPort(host, port)
	case strings.Contains(host, ":"):
		u.Host = "[" + host + "]"
	default:
		u.Host = host
	}
	value := u.String()
	return &value, nil
}
