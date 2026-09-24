package config

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// BrowserAuth configures the HTTP service only; workers never load SAML keys.
type BrowserAuth struct {
	Mode         string
	PublicURL    string
	EntityID     string
	MetadataFile string
	CertFile     string
	KeyFile      string
	SessionTTL   time.Duration
	ttlSeconds   string
}

func (c BrowserAuth) Validated() (BrowserAuth, error) {
	switch c.Mode {
	case "api_only", "anonymous", "saml":
	default:
		return c, errors.New("ORPHEUS_BROWSER_AUTH must be api_only, saml or anonymous")
	}
	if c.ttlSeconds != "" {
		n, err := strconv.Atoi(c.ttlSeconds)
		if err != nil || n < 300 || n > 86400 {
			return c, errors.New("BROWSER_SESSION_TTL_SECONDS must be between 300 and 86400")
		}
		c.SessionTTL = time.Duration(n) * time.Second
	}
	if c.SessionTTL < 5*time.Minute || c.SessionTTL > 24*time.Hour {
		return c, errors.New("invalid browser session TTL")
	}
	if c.PublicURL != "" {
		u, err := url.Parse(c.PublicURL)
		if err != nil || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(c.PublicURL, "#") || (u.Scheme != "http" && u.Scheme != "https") {
			return c, errors.New("ORPHEUS_PUBLIC_URL must be an HTTP(S) origin without path, query, credentials or fragment")
		}
		// Browser Origin serialization lowercases the host and omits default ports.
		host, port := strings.ToLower(u.Hostname()), u.Port()
		if port != "" {
			n, err := strconv.Atoi(port)
			if err != nil || n < 1 || n > 65535 {
				return c, errors.New("invalid ORPHEUS_PUBLIC_URL port")
			}
			port = strconv.Itoa(n)
		}
		if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
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
		c.PublicURL = u.String()
		if c.Mode == "saml" && u.Scheme != "https" {
			return c, errors.New("SAML requires an HTTPS ORPHEUS_PUBLIC_URL")
		}
	}
	if c.Mode == "saml" && (c.PublicURL == "" || strings.TrimSpace(c.EntityID) == "" || c.MetadataFile == "" || c.CertFile == "" || c.KeyFile == "") {
		return c, errors.New("SAML requires public URL, entity ID, metadata, SP certificate and private key")
	}
	return c, nil
}
