package secret

import (
	"bytes"
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
	sid, rid := uuid.New(), uuid.New()
	want := map[string]string{"TOKEN": "secret-value", "EMPTY": "", "UNICODE": "значение"}
	for _, run := range []bool{false, true} {
		t.Run(map[bool]string{false: "session", true: "run"}[run], func(t *testing.T) {
			encrypt := func() (*Value, error) {
				if run {
					return c.EncryptRun(sid, rid, want)
				}
				return c.Encrypt(sid, want)
			}
			decrypt := func(cipher *Cipher, v *Value) (map[string]string, error) {
				if run {
					return cipher.DecryptRun(sid, rid, v)
				}
				return cipher.Decrypt(sid, v)
			}
			a, err := encrypt()
			if err != nil {
				t.Fatal(err)
			}
			b, err := encrypt()
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(a.Nonce, b.Nonce) || bytes.Equal(a.Ciphertext, b.Ciphertext) || bytes.Contains(a.Ciphertext, []byte(want["TOKEN"])) {
				t.Fatal("encryption lacks randomization or leaks plaintext")
			}
			restarted, err := New(key)
			if err != nil {
				t.Fatal(err)
			}
			got, err := decrypt(restarted, a)
			if err != nil || !maps.Equal(got, want) {
				t.Fatal(got, err)
			}
			if _, err := c.Decrypt(uuid.New(), a); err == nil {
				t.Fatal("accepted another session")
			}
			for _, ids := range [][2]uuid.UUID{{uuid.New(), rid}, {sid, uuid.New()}} {
				if _, err := c.DecryptRun(ids[0], ids[1], a); err == nil {
					t.Fatal("accepted another run or session")
				}
			}
			if run {
				if _, err := c.Decrypt(sid, a); err == nil {
					t.Fatal("accepted run value in session scope")
				}
			} else {
				if _, err := c.DecryptRun(sid, rid, a); err == nil {
					t.Fatal("accepted session value in run scope")
				}
			}
			other, _ := New(base64.URLEncoding.EncodeToString([]byte(strings.Repeat("a", 32))))
			if _, err := decrypt(other, a); err == nil {
				t.Fatal("accepted wrong key")
			}
			a.Ciphertext[0] ^= 1
			if _, err := decrypt(c, a); err == nil {
				t.Fatal("accepted tampering")
			}
			for _, v := range []*Value{{}, {Nonce: make([]byte, 11), Ciphertext: make([]byte, 16)}, {Nonce: make([]byte, 13), Ciphertext: make([]byte, 16)}, {Nonce: make([]byte, 12), Ciphertext: make([]byte, 15)}} {
				if _, err := decrypt(c, v); err == nil {
					t.Fatal("accepted invalid lengths")
				}
			}
		})
	}
	for _, run := range []bool{false, true} {
		var v *Value
		if run {
			v, err = c.EncryptRun(sid, rid, nil)
		} else {
			v, err = c.Encrypt(sid, nil)
		}
		if err != nil || v != nil {
			t.Fatal("empty environment stored")
		}
	}
	if _, err := New("bad"); err == nil {
		t.Fatal("accepted invalid key")
	}
}
