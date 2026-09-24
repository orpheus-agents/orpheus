//go:build integration

package testutil

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/beevik/etree"
	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/config"
	dsig "github.com/russellhaering/goxmldsig"
)

// SAMLFixture signs fresh responses using a test-only IdP certificate.
type SAMLFixture struct {
	Config      config.BrowserAuth
	Key         *rsa.PrivateKey
	Certificate []byte
}

func NewSAML(t *testing.T, origin string) *SAMLFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "SAML fixture"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	cert, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	c := config.DefaultSettings().BrowserAuth
	c.Mode = "saml"
	c.PublicURL = origin
	c.EntityID = "orpheus-test"
	c.CertFile = filepath.Join(dir, "sp.crt")
	c.KeyFile = filepath.Join(dir, "sp.key")
	c.MetadataFile = filepath.Join(dir, "idp.xml")
	for path, raw := range map[string][]byte{
		c.CertFile:     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}),
		c.KeyFile:      pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
		c.MetadataFile: []byte(fmt.Sprintf(`<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="https://idp.example.test"><IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol"><KeyDescriptor use="signing"><KeyInfo xmlns="http://www.w3.org/2000/09/xmldsig#"><X509Data><X509Certificate>%s</X509Certificate></X509Data></KeyInfo></KeyDescriptor><SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="https://idp.example.test/sso"/></IDPSSODescriptor></EntityDescriptor>`, base64.StdEncoding.EncodeToString(cert))),
	} {
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return &SAMLFixture{Config: c, Key: key, Certificate: cert}
}
func (f *SAMLFixture) Response(t *testing.T, requestID string, edit func(*etree.Element)) string {
	t.Helper()
	now := time.Now().UTC()
	instant := now.Format(time.RFC3339Nano)
	expiry := now.Add(5 * time.Minute).Format(time.RFC3339Nano)
	raw := fmt.Sprintf(`<samlp:Response xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol" xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion" ID="_%s" Version="2.0" IssueInstant="%s" InResponseTo="%s" Destination="%s/auth/callback">
<saml:Issuer>https://idp.example.test</saml:Issuer><samlp:Status><samlp:StatusCode Value="urn:oasis:names:tc:SAML:2.0:status:Success"/></samlp:Status>
<saml:Assertion ID="_%s" Version="2.0" IssueInstant="%s"><saml:Issuer>https://idp.example.test</saml:Issuer>
<saml:Subject><saml:NameID>fixture-subject</saml:NameID><saml:SubjectConfirmation Method="urn:oasis:names:tc:SAML:2.0:cm:bearer"><saml:SubjectConfirmationData InResponseTo="%s" Recipient="%s/auth/callback" NotOnOrAfter="%s"/></saml:SubjectConfirmation></saml:Subject>
<saml:Conditions NotBefore="%s" NotOnOrAfter="%s"><saml:AudienceRestriction><saml:Audience>%s</saml:Audience></saml:AudienceRestriction></saml:Conditions>
<saml:AuthnStatement AuthnInstant="%s" SessionNotOnOrAfter="%s"><saml:AuthnContext><saml:AuthnContextClassRef>urn:oasis:names:tc:SAML:2.0:ac:classes:PasswordProtectedTransport</saml:AuthnContextClassRef></saml:AuthnContext></saml:AuthnStatement>
<saml:AttributeStatement><saml:Attribute Name="preferred_username"><saml:AttributeValue>operator</saml:AttributeValue></saml:Attribute></saml:AttributeStatement></saml:Assertion></samlp:Response>`, uuid.NewString(), instant, requestID, f.Config.PublicURL, uuid.NewString(), instant, requestID, f.Config.PublicURL, expiry, now.Add(-time.Minute).Format(time.RFC3339Nano), expiry, f.Config.EntityID, instant, now.Add(time.Hour).Format(time.RFC3339Nano))
	doc := etree.NewDocument()
	if err := doc.ReadFromString(raw); err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(doc.Root())
	}
	signing, err := dsig.NewSigningContext(f.Key, [][]byte{f.Certificate})
	if err != nil {
		t.Fatal(err)
	}
	signing.IdAttribute = "ID"
	signing.Canonicalizer = dsig.MakeC14N10ExclusiveCanonicalizerWithPrefixList("")
	if err := signing.SetSignatureMethod(dsig.RSASHA256SignatureMethod); err != nil {
		t.Fatal(err)
	}
	signed, err := signing.SignEnveloped(doc.Root())
	if err != nil {
		t.Fatal(err)
	}
	doc.SetRoot(signed)
	data, err := doc.WriteToBytes()
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(data)
}
