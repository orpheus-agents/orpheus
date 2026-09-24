// Package browserauth implements read-only browser identity and SP-initiated SAML.
// XML/signature validation belongs to crewjam/saml; opaque credentials live in PostgreSQL.
package browserauth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/crewjam/saml"
	"github.com/crewjam/saml/samlsp"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store/db"
	dsig "github.com/russellhaering/goxmldsig"
)

const CookieName = "__Host-orpheus_session"
const noncePrefix = "__Host-orpheus_saml_"
const CallbackLimit = 1 << 20
const skew = time.Minute

var ErrNoSession = errors.New("browser session absent")

type Service struct {
	config config.BrowserAuth
	pool   *pgxpool.Pool
	sp     *saml.ServiceProvider
}

type Identity struct {
	TokenHash   []byte
	DisplayName string
	ExpiresAt   time.Time
}

type State struct {
	Mode          string     `json:"mode"`
	Authenticated bool       `json:"authenticated"`
	ReadAccess    bool       `json:"read_access"`
	User          *User      `json:"user"`
	ExpiresAt     *time.Time `json:"expires_at"`
}
type User struct {
	DisplayName string `json:"display_name"`
}

func New(c config.BrowserAuth, pool *pgxpool.Pool) (*Service, error) {
	c, err := c.Validated()
	if err != nil {
		return nil, err
	}
	s := &Service{config: c, pool: pool}
	if c.Mode != "saml" {
		return s, nil
	}
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, errors.New("invalid SAML SP certificate or private key")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil || time.Now().Before(leaf.NotBefore) || !time.Now().Before(leaf.NotAfter) {
		return nil, errors.New("invalid or expired SAML SP certificate")
	}
	signer, ok := cert.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("unsupported SAML SP key")
	}
	signature := dsig.RSASHA256SignatureMethod
	switch signer.(type) {
	case *rsa.PrivateKey:
	case *ecdsa.PrivateKey:
		signature = dsig.ECDSASHA256SignatureMethod
	default:
		return nil, errors.New("SAML requires an RSA or ECDSA SP key")
	}
	raw, err := os.ReadFile(c.MetadataFile)
	if err != nil {
		return nil, errors.New("cannot read SAML IdP metadata")
	}
	metadata, err := samlsp.ParseMetadata(raw)
	if err != nil || metadata.EntityID == "" || len(metadata.IDPSSODescriptors) == 0 {
		return nil, errors.New("invalid SAML IdP metadata")
	}
	// Preserve the enclosing EntitiesDescriptor expiry that ParseMetadata omits.
	var envelope struct {
		ValidUntil *time.Time `xml:"validUntil,attr"`
	}
	if err := xml.Unmarshal(raw, &envelope); err != nil {
		return nil, errors.New("invalid SAML metadata expiry")
	}
	if envelope.ValidUntil != nil && (metadata.ValidUntil.IsZero() || envelope.ValidUntil.Before(metadata.ValidUntil)) {
		metadata.ValidUntil = *envelope.ValidUntil
	}
	signingCertificates := 0
	for _, descriptor := range metadata.IDPSSODescriptors {
		for _, key := range descriptor.KeyDescriptors {
			if key.Use != "" && key.Use != "signing" {
				continue
			}
			for _, certificate := range key.KeyInfo.X509Data.X509Certificates {
				der, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(certificate.Data), ""))
				if err != nil {
					return nil, errors.New("invalid SAML IdP signing certificate")
				}
				if _, err := x509.ParseCertificate(der); err != nil {
					return nil, errors.New("invalid SAML IdP signing certificate")
				}
				signingCertificates++
			}
		}
	}
	if signingCertificates == 0 {
		return nil, errors.New("SAML metadata requires an IdP signing certificate")
	}
	origin, _ := url.Parse(c.PublicURL)
	sp := &saml.ServiceProvider{AuthnNameIDFormat: saml.UnspecifiedNameIDFormat, EntityID: c.EntityID, Key: signer, Certificate: leaf, IDPMetadata: metadata,
		MetadataURL: *origin.ResolveReference(&url.URL{Path: "/saml/metadata"}), AcsURL: *origin.ResolveReference(&url.URL{Path: "/auth/callback"}), SignatureMethod: signature}
	// Requiring a matching audience closes the library's permissive empty-list default.
	sp.ValidateAudienceRestriction = func(a *saml.Assertion) error {
		if a.Conditions == nil || len(a.Conditions.AudienceRestrictions) == 0 {
			return errors.New("missing audience")
		}
		for _, restriction := range a.Conditions.AudienceRestrictions {
			if restriction.Audience.Value != c.EntityID {
				return errors.New("invalid audience")
			}
		}
		return nil
	}
	s.sp = sp
	destination, err := url.Parse(sp.GetSSOBindingLocation(saml.HTTPRedirectBinding))
	if err != nil || destination.Scheme != "https" || destination.Host == "" || destination.User != nil {
		return nil, errors.New("IdP metadata requires an HTTPS Redirect SSO endpoint")
	}
	return s, nil
}

func (s *Service) Mode() string { return s.config.Mode }
func (s *Service) metadataValid() error {
	expiry := s.sp.IDPMetadata.ValidUntil
	if !expiry.IsZero() && !time.Now().Before(expiry) {
		return session.Problem(503, "auth_unavailable", "SAML metadata has expired.")
	}
	for _, descriptor := range s.sp.IDPMetadata.IDPSSODescriptors {
		if descriptor.ValidUntil != nil && !time.Now().Before(*descriptor.ValidUntil) {
			return session.Problem(503, "auth_unavailable", "SAML metadata has expired.")
		}
	}
	return nil
}
func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
func tokenHash(token string) []byte { h := sha256.Sum256([]byte(token)); return h[:] }
func validToken(token string) bool {
	b, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(b) == 32 && base64.RawURLEncoding.EncodeToString(b) == token
}
func cookie(w http.ResponseWriter, name, value string, expires time.Time, sameSite http.SameSite) {
	age := int(time.Until(expires).Seconds())
	if value == "" {
		age = -1
		expires = time.Unix(1, 0)
	} else {
		age = max(age, 1)
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: sameSite, Expires: expires, MaxAge: age})
}
func ClearCookie(w http.ResponseWriter) { cookie(w, CookieName, "", time.Time{}, http.SameSiteLaxMode) }
func requestHash(r *http.Request, name string) []byte {
	cookies := r.CookiesNamed(name)
	if len(cookies) != 1 || !validToken(cookies[0].Value) {
		return nil
	}
	return tokenHash(cookies[0].Value)
}
func (s *Service) Identity(ctx context.Context, hash []byte) (*Identity, error) {
	if s.config.Mode != "saml" || hash == nil {
		return nil, ErrNoSession
	}
	row, err := db.New(s.pool).GetBrowserSession(ctx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, err
	}
	return &Identity{TokenHash: row.TokenHash, DisplayName: row.DisplayName, ExpiresAt: row.ExpiresAt}, nil
}
func (s *Service) Authenticate(r *http.Request) (*Identity, error) {
	return s.Identity(r.Context(), requestHash(r, CookieName))
}
func (s *Service) State(w http.ResponseWriter, r *http.Request) (State, error) {
	state := State{Mode: s.Mode(), ReadAccess: s.Mode() == "anonymous"}
	identity, err := s.Authenticate(r)
	if errors.Is(err, ErrNoSession) {
		if len(r.CookiesNamed(CookieName)) > 0 {
			ClearCookie(w)
		}
		return state, nil
	}
	if err != nil {
		return state, err
	}
	state.Authenticated = true
	state.ReadAccess = true
	state.User = &User{DisplayName: identity.DisplayName}
	state.ExpiresAt = &identity.ExpiresAt
	return state, nil
}

func returnPath(raw string) (string, error) {
	if raw == "" {
		return "/api/v1/auth/session", nil
	}
	bad := func() (string, error) {
		return "", session.Problem(400, "invalid_return_path", "A local return path is required.")
	}
	if len(raw) > 2048 {
		return bad()
	}
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" || u.Opaque != "" || u.Fragment != "" {
		return bad()
	}
	// Query values are opaque to the return-path policy. Keep encoded spaces,
	// percent signs and URLs in filters, but never permit literal control bytes.
	for _, c := range raw {
		if c < 32 || c == 127 {
			return bad()
		}
	}
	query, err := url.QueryUnescape(u.RawQuery)
	if err != nil {
		return bad()
	}
	for _, c := range query {
		if c < 32 || c == 127 {
			return bad()
		}
	}
	// Decoded path validation prevents encoded separators and auth-loop bypasses.
	if !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") || strings.ContainsAny(u.Path, "\\%") {
		return bad()
	}
	for _, c := range u.Path {
		if c <= 32 || c == 127 {
			return bad()
		}
	}
	for segment := range strings.SplitSeq(u.Path, "/") {
		if segment == "." || segment == ".." {
			return bad()
		}
	}
	if u.Path == "/auth" || strings.HasPrefix(u.Path, "/auth/") || u.Path == "/saml" || strings.HasPrefix(u.Path, "/saml/") {
		return bad()
	}
	return raw, nil
}
func (s *Service) requireSAML() error {
	if s.Mode() != "saml" {
		return session.Problem(404, "auth_not_enabled", "SAML authentication is not enabled.")
	}
	return s.metadataValid()
}
func (s *Service) Login(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireSAML(); err != nil {
		return err
	}
	if len(r.URL.Query()["next"]) > 1 {
		return session.Problem(400, "invalid_return_path", "Return path must occur once.")
	}
	next, err := returnPath(r.URL.Query().Get("next"))
	if err != nil {
		return err
	}
	request, err := s.sp.MakeAuthenticationRequest(s.sp.GetSSOBindingLocation(saml.HTTPRedirectBinding), saml.HTTPRedirectBinding, saml.HTTPPostBinding)
	if err != nil {
		return session.Problem(503, "auth_unavailable", "Cannot start SAML login.")
	}
	relay, nonce := randomToken(), randomToken()
	target, err := request.Redirect(relay, s.sp)
	if err != nil {
		return session.Problem(503, "auth_unavailable", "Cannot start SAML login.")
	}
	expiry := time.Now().Add(10 * time.Minute)
	if err := db.New(s.pool).InsertBrowserLogin(r.Context(), db.InsertBrowserLoginParams{ID: relay, RequestID: request.ID, BrowserNonceHash: tokenHash(nonce), ReturnPath: next, ExpiresAt: expiry, PreviousSessionHash: requestHash(r, CookieName)}); err != nil {
		return err
	}
	cookie(w, noncePrefix+relay, nonce, expiry, http.SameSiteNoneMode)
	w.Header().Set("Location", target.String())
	w.WriteHeader(http.StatusFound)
	return nil
}
func invalidState() error {
	return session.Problem(400, "invalid_auth_state", "Login request is invalid or expired.")
}
func invalidAssertion() error {
	return session.Problem(401, "invalid_saml_response", "SAML response is invalid.")
}
func (s *Service) Callback(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireSAML(); err != nil {
		return err
	}
	r.Body = http.MaxBytesReader(w, r.Body, CallbackLimit)
	if err := r.ParseForm(); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return session.Problem(413, "request_too_large", "SAML response is too large.")
		}
		return invalidState()
	}
	if len(r.PostForm["RelayState"]) != 1 || len(r.PostForm["SAMLResponse"]) != 1 || len(r.URL.Query()) != 0 {
		return invalidState()
	}
	relay := r.PostForm.Get("RelayState")
	if !validToken(relay) {
		return invalidState()
	}
	hash := requestHash(r, noncePrefix+relay)
	if hash == nil {
		return invalidState()
	}
	pending, err := db.New(s.pool).GetBrowserLogin(r.Context(), db.GetBrowserLoginParams{ID: relay, BrowserNonceHash: hash})
	if errors.Is(err, pgx.ErrNoRows) {
		return invalidState()
	}
	if err != nil {
		return err
	}
	assertion, err := s.sp.ParseResponse(r, []string{pending.RequestID})
	if err != nil {
		slog.WarnContext(r.Context(), "SAML callback rejected", "reason", samlFailureReason(err))
		return invalidAssertion()
	}
	if err := validateAssertion(assertion, time.Now()); err != nil {
		slog.WarnContext(r.Context(), "SAML callback rejected", "reason", "assertion_conditions")
		return err
	}
	expiry := time.Now().Add(s.config.SessionTTL)
	for _, statement := range assertion.AuthnStatements {
		if statement.SessionNotOnOrAfter != nil && statement.SessionNotOnOrAfter.Before(expiry) {
			expiry = *statement.SessionNotOnOrAfter
		}
	}
	if !expiry.After(time.Now()) {
		return invalidAssertion()
	}
	name := assertion.Subject.NameID.Value
	for _, wanted := range []string{"preferred_username", "email"} {
		found := ""
		for _, statement := range assertion.AttributeStatements {
			for _, attr := range statement.Attributes {
				if (attr.Name == wanted || attr.FriendlyName == wanted) && len(attr.Values) > 0 {
					found = attr.Values[0].Value
				}
			}
		}
		if found != "" {
			name = found
			break
		}
	}
	token := randomToken()
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	q := db.New(tx)
	consumed, err := q.ConsumeBrowserLogin(r.Context(), db.ConsumeBrowserLoginParams{ID: relay, BrowserNonceHash: hash})
	if errors.Is(err, pgx.ErrNoRows) {
		return invalidState()
	}
	if err != nil {
		return err
	}
	// The login GET captures the Lax cookie that a cross-site IdP POST omits.
	if err := q.DeleteBrowserSession(r.Context(), consumed.PreviousSessionHash); err != nil {
		return err
	}
	// A same-site callback may also carry a session issued by another login tab.
	if err := q.DeleteBrowserSession(r.Context(), requestHash(r, CookieName)); err != nil {
		return err
	}
	if err := q.InsertBrowserSession(r.Context(), db.InsertBrowserSessionParams{TokenHash: tokenHash(token), Subject: assertion.Subject.NameID.Value, DisplayName: name, ExpiresAt: expiry}); err != nil {
		return err
	}
	if err := tx.Commit(r.Context()); err != nil {
		return err
	}
	cookie(w, noncePrefix+relay, "", time.Time{}, http.SameSiteNoneMode)
	cookie(w, CookieName, token, expiry, http.SameSiteLaxMode)
	w.Header().Set("Location", consumed.ReturnPath)
	w.WriteHeader(http.StatusSeeOther)
	return nil
}

// Tighten the library's default 180s clock tolerance without mutating global state.
func validateAssertion(a *saml.Assertion, now time.Time) error {
	if a.Subject == nil || a.Subject.NameID == nil || a.Subject.NameID.Value == "" || len(a.Subject.SubjectConfirmations) == 0 || a.Conditions == nil || len(a.AuthnStatements) == 0 {
		return invalidAssertion()
	}
	if a.Conditions.NotBefore.After(now.Add(skew)) || !now.Before(a.Conditions.NotOnOrAfter.Add(skew)) {
		return invalidAssertion()
	}
	for _, confirmation := range a.Subject.SubjectConfirmations {
		if confirmation.SubjectConfirmationData == nil || confirmation.Method != "urn:oasis:names:tc:SAML:2.0:cm:bearer" || !now.Before(confirmation.SubjectConfirmationData.NotOnOrAfter.Add(skew)) {
			return invalidAssertion()
		}
	}
	return nil
}
func (s *Service) Logout(w http.ResponseWriter, r *http.Request) error {
	if s.config.PublicURL == "" || r.Header.Get("Origin") != s.config.PublicURL || r.Header.Get("X-Orpheus-CSRF") != "1" {
		return session.Problem(403, "invalid_origin", "A same-origin logout request is required.")
	}
	if s.Mode() == "saml" {
		if err := db.New(s.pool).DeleteBrowserSession(r.Context(), requestHash(r, CookieName)); err != nil {
			return err
		}
	}
	ClearCookie(w)
	w.WriteHeader(http.StatusNoContent)
	return nil
}
func (s *Service) Metadata(w http.ResponseWriter, _ *http.Request) error {
	if err := s.requireSAML(); err != nil {
		return err
	}
	raw, err := xml.Marshal(s.sp.Metadata())
	if err != nil {
		return session.Problem(503, "auth_unavailable", "Cannot generate SAML metadata.")
	}
	w.Header().Set("Content-Type", "application/samlmetadata+xml")
	_, err = w.Write(raw)
	return err
}

// Cleanup runs independently of request authorization. Every batch has its own
// short statement and SKIP LOCKED lets replicas share expiry cleanup safely.
func Cleanup(ctx context.Context, pool *pgxpool.Pool) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Minute):
			for _, clean := range []func(context.Context) (int64, error){db.New(pool).CleanupBrowserLogins, db.New(pool).CleanupBrowserSessions} {
				for ctx.Err() == nil {
					batch, cancel := context.WithTimeout(ctx, 5*time.Second)
					n, err := clean(batch)
					cancel()
					if err != nil || n < 1000 {
						break
					}
				}
			}
		}
	}
}

// Library errors can contain assertions, identifiers and attacker-supplied XML.
// Only emit a fixed category, never PrivateErr or InvalidResponseError.Response.
func samlFailureReason(err error) string {
	private, ok := errors.AsType[*saml.InvalidResponseError](err)
	if !ok || private.PrivateErr == nil {
		return "invalid_response"
	}
	if _, ok := errors.AsType[saml.ErrBadStatus](private.PrivateErr); ok {
		return "idp_status"
	}
	message := strings.ToLower(private.PrivateErr.Error())
	// Match only the library's fixed prefixes, not values quoted inside errors.
	for _, rule := range []struct{ prefix, reason string }{
		{"audience restriction validation failed:", "audience"},
		{"assertion conditions audiencerestriction ", "audience"},
		{"response issuer ", "issuer"}, {"issuer is not ", "issuer"},
		{"assertion subjectconfirmation recipient ", "recipient"},
		{"`destination` does not match ", "destination"},
		{"`inresponseto` does not match ", "request_id"},
		{"assertion subjectconfirmation one of the possible request ids ", "request_id"},
		{"expired on ", "expired"}, {"response issueinstant expired ", "expired"},
		{"assertion subjectconfirmationdata is expired", "expired"},
		{"assertion conditions is expired", "expired"},
		{"assertion conditions is not yet valid", "not_yet_valid"},
		{"cannot validate signature on ", "signature"},
		{"signature element not present", "signature"},
		{"signature validation failed", "signature"},
		{"cannot parse base64:", "encoding"},
		{"invalid xml:", "xml"}, {"cannot unmarshal response:", "xml"},
		{"plaintext response contains invalid xml:", "xml"},
		{"cannot parse plaintext response ", "xml"},
	} {
		if strings.HasPrefix(message, rule.prefix) {
			return rule.reason
		}
	}
	return "invalid_response"
}
