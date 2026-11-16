package diagnostic

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestRedactorMasksKnownValuesAndCredentialFormats(t *testing.T) {
	const secret = `private/value+with"quotes&chars`
	encoded, _ := json.Marshal(secret)
	r := new(Redactor)
	r.Add(secret, "first-private-line\nsecond-private-line")
	for _, input := range []string{
		secret, string(encoded[1 : len(encoded)-1]), url.QueryEscape(secret), url.PathEscape(secret),
		"first-private-line", "second-private-line", "Bearer unregistered-access-value",
		"Basic dXNlcjpwYXNzd29yZA==", `{"refresh_token":"newly-rotated-value"}`,
		`{"token":"prefix\"newly-rotated-value"}`, `password='prefix\'confidential'`,
		"password = confidential", "API_KEY=confidential", "https://user:password@example.test/path?token=unseen",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.privateSignature",
		"-----BEGIN PRIVATE KEY-----\nprivate-key-payload\n-----END PRIVATE KEY-----",
	} {
		t.Run(input, func(t *testing.T) {
			clean, ok := r.Clean(input)
			if !ok || !strings.Contains(clean, redacted) {
				t.Fatalf("not redacted: %q", clean)
			}
			for _, forbidden := range []string{secret, "unregistered-access-value", "newly-rotated-value", "confidential", "private-key-payload", "first-private-line", "second-private-line", "privateSignature", "example.test"} {
				if strings.Contains(clean, forbidden) {
					t.Fatalf("leaked %q in %q", forbidden, clean)
				}
			}
		})
	}
	clean, ok := r.Clean("\x1b[31mworkspace\x1b[0m routing discovery failed\x00\xff")
	if !ok || clean != "workspace routing discovery failed�" {
		t.Fatalf("normalization: %q", clean)
	}
}

func TestRedactorKeepsRotatedAndShortSecrets(t *testing.T) {
	r := new(Redactor)
	r.Add("old-secret", "t")
	r.Add("new-secret")
	clean, ok := r.Clean("old-secret new-secret raw=t Authorization: Bearer fresh-unregistered")
	if !ok || strings.Contains(clean, "old-secret") || strings.Contains(clean, "new-secret") || strings.Contains(clean, "raw=t") || strings.Contains(clean, "fresh-unregistered") {
		t.Fatalf("secret leaked after rotation: %q", clean)
	}
}

func TestStderrAppliesRotatedSecretsToBufferedText(t *testing.T) {
	r := new(Redactor)
	r.Add("old-secret")
	s := NewStderr(r, false)
	s.Append([]byte("old-secret newly-seen-secret\n"))
	r.Add("newly-seen-secret")
	text, _, _ := s.Snapshot()
	if text == nil || strings.Contains(*text, "secret") {
		t.Fatal("rotation leaked buffered text", text)
	}
}

func TestRedactorFailsClosedAtMemoryLimit(t *testing.T) {
	r := new(Redactor)
	tail := NewStderr(r, false)
	tail.Append([]byte("previously clean\n"))
	r.Add(strings.Repeat("s", maxSecretBytes+1))
	if !r.Blocked() {
		t.Fatal("unbounded secret registry")
	}
	if text, ok := r.Clean("safe"); ok || text != "" {
		t.Fatal("text allowed after overflow")
	}
	if text, _, omitted := tail.Snapshot(); text != nil || len(omitted) != 1 || omitted[0] != "redaction_limit" {
		t.Fatal("old tail survived overflow", omitted)
	}
}

func TestStderrRedactsAcrossEveryChunkBoundary(t *testing.T) {
	const line = "failure access-token-without-a-label\n"
	for split := 0; split <= len(line); split++ {
		r := new(Redactor)
		r.Add("access-token-without-a-label")
		s := NewStderr(r, false)
		s.Append([]byte(line[:split]))
		s.Append([]byte(line[split:]))
		text, truncated, omitted := s.Snapshot()
		if text == nil || *text != "failure [REDACTED]\n" || truncated || len(omitted) != 0 {
			t.Fatalf("split %d: %v %t %v", split, text, truncated, omitted)
		}
	}
}

func TestStderrBoundsAndPartialAttachment(t *testing.T) {
	r := new(Redactor)
	s := NewStderr(r, true)
	s.Append([]byte("fragment-without-start\nuseful\n"))
	s.Append([]byte(strings.Repeat("x", MaxInputBytes+1)))
	s.Append([]byte("private-suffix\n-----BEGIN PRIVATE KEY-----\nraw-key\n-----END PRIVATE KEY-----\nlast"))
	s.End()
	text, truncated, omitted := s.Snapshot()
	if text == nil || !truncated || len(omitted) != 1 || !strings.Contains(*text, "useful\n") || !strings.HasSuffix(*text, "last\n") {
		t.Fatal(text, truncated, omitted)
	}
	for _, value := range []string{"fragment-without-start", "private-suffix", "raw-key", strings.Repeat("x", 20)} {
		if strings.Contains(*text, value) {
			t.Fatal("unsafe line fragment leaked", value)
		}
	}
	for range 1000 {
		s.Append([]byte(strings.Repeat("y", 100) + " useful\n"))
	}
	text, truncated, _ = s.Snapshot()
	if len(*text) > MaxStderrBytes || !truncated || !strings.HasSuffix(*text, " useful\n") {
		t.Fatal("tail not bounded")
	}
}
