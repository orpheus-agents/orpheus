package secret

import (
	"encoding/base64"
	"maps"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestCipher(t *testing.T) {
	key := base64.URLEncoding.EncodeToString(make([]byte, 32))
	c, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	env := map[string]string{"TOKEN": "secret-value"}
	a, err := c.Encrypt(id, env)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Encrypt(id, env)
	if err != nil {
		t.Fatal(err)
	}
	if *a == *b || strings.Contains(*a, env["TOKEN"]) {
		t.Fatal("encryption lacks randomization or leaks plaintext")
	}
	restarted, _ := New(key)
	got, err := restarted.Decrypt(id, a)
	if err != nil || !maps.Equal(got, env) {
		t.Fatalf("restart: %v %v", got, err)
	}
	if _, err := c.Decrypt(uuid.New(), a); err == nil {
		t.Fatal("accepted another session")
	}
	raw := []byte(*a)
	raw[len(raw)/2] ^= 1
	if _, err := c.Decrypt(id, new(string(raw))); err == nil {
		t.Fatal("accepted tampering")
	}
	for _, token := range []string{"fernet-v1:abc", "aes-gcm-v1:abc", "bad"} {
		if _, err := c.Decrypt(id, &token); err == nil {
			t.Fatal("accepted invalid ciphertext")
		}
	}
	empty, err := c.Encrypt(id, nil)
	if err != nil || empty != nil {
		t.Fatal("empty environment must not store ciphertext")
	}
	if _, err := New("bad"); err == nil {
		t.Fatal("accepted invalid key")
	}
	other, _ := New(base64.URLEncoding.EncodeToString([]byte(strings.Repeat("a", 32))))
	if _, err := other.Decrypt(id, a); err == nil {
		t.Fatal("accepted wrong key")
	}
}

func TestRunCipherBindsBothIDsAndScope(t *testing.T) {
	c, err := New(base64.URLEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	sid, rid := uuid.New(), uuid.New()
	want := map[string]string{"TOKEN": "run-secret"}
	token, err := c.EncryptRun(sid, rid, want)
	if err != nil || token == nil || !strings.HasPrefix(*token, "aes-gcm-run-v1:") || strings.Contains(*token, want["TOKEN"]) {
		t.Fatal(token, err)
	}
	restarted, err := New(base64.URLEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := restarted.DecryptRun(sid, rid, token)
	if err != nil || !maps.Equal(got, want) {
		t.Fatal(got, err)
	}
	for _, ids := range [][2]uuid.UUID{{uuid.New(), rid}, {sid, uuid.New()}} {
		if _, err := c.DecryptRun(ids[0], ids[1], token); err == nil {
			t.Fatal("accepted ciphertext for another run or session")
		}
	}
	if _, err := c.Decrypt(sid, token); err == nil {
		t.Fatal("accepted run ciphertext as session ciphertext")
	}
	sessionToken, err := c.Encrypt(sid, want)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.DecryptRun(sid, rid, sessionToken); err == nil {
		t.Fatal("accepted session ciphertext as run ciphertext")
	}
	empty, err := c.EncryptRun(sid, rid, nil)
	if err != nil || empty != nil {
		t.Fatal("empty environment must not store ciphertext")
	}
}
