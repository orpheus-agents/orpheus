// Package secret encrypts explicit sandbox environment values at rest.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
)

type Cipher struct{ aead cipher.AEAD }

// Value is one authenticated encrypted payload. The owner supplies the AAD.
type Value struct {
	Nonce      []byte
	Ciphertext []byte
}

func New(key string) (*Cipher, error) {
	raw, err := base64.URLEncoding.DecodeString(key)
	if err != nil || len(raw) != 32 {
		return nil, errors.New("ENV_ENCRYPTION_KEY must be a base64url encoded 32-byte key")
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

func (c *Cipher) Encrypt(id uuid.UUID, env map[string]string) (*Value, error) {
	return c.encrypt(id[:], env)
}

func runAAD(sessionID, runID uuid.UUID) []byte {
	aad := []byte("orpheus-run-env-v1\x00")
	aad = append(aad, sessionID[:]...)
	return append(aad, runID[:]...)
}

func (c *Cipher) EncryptRun(sessionID, runID uuid.UUID, env map[string]string) (*Value, error) {
	return c.encrypt(runAAD(sessionID, runID), env)
}

func (c *Cipher) encrypt(aad []byte, env map[string]string) (*Value, error) {
	if len(env) == 0 {
		return nil, nil
	}
	data, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return &Value{Nonce: nonce, Ciphertext: c.aead.Seal(nil, nonce, data, aad)}, nil
}

func (c *Cipher) Decrypt(id uuid.UUID, value *Value) (map[string]string, error) {
	return c.decrypt(id[:], value)
}

func (c *Cipher) DecryptRun(sessionID, runID uuid.UUID, value *Value) (map[string]string, error) {
	return c.decrypt(runAAD(sessionID, runID), value)
}

func (c *Cipher) decrypt(aad []byte, value *Value) (map[string]string, error) {
	values := map[string]string{}
	if value == nil {
		return values, nil
	}
	if len(value.Nonce) != c.aead.NonceSize() || len(value.Ciphertext) < c.aead.Overhead() {
		return nil, errors.New("invalid ciphertext")
	}
	plain, err := c.aead.Open(nil, value.Nonce, value.Ciphertext, aad)
	if err != nil {
		return nil, errors.New("environment cannot be decrypted")
	}
	if err := json.Unmarshal(plain, &values); err != nil {
		return nil, errors.New("invalid encrypted environment")
	}
	return values, nil
}

// SessionToken and RunToken preserve the legacy column format during dual writes.
func (v *Value) SessionToken() *string { return v.token("aes-gcm-v1:") }
func (v *Value) RunToken() *string     { return v.token("aes-gcm-run-v1:") }

func (v *Value) token(prefix string) *string {
	if v == nil {
		return nil
	}
	data := append(append([]byte(nil), v.Nonce...), v.Ciphertext...)
	return new(prefix + base64.RawURLEncoding.EncodeToString(data))
}

func ParseSession(token *string) (*Value, error) { return parse("aes-gcm-v1:", token) }
func ParseRun(token *string) (*Value, error)     { return parse("aes-gcm-run-v1:", token) }

func parse(prefix string, ciphertext *string) (*Value, error) {
	if ciphertext == nil {
		return nil, nil
	}
	token, ok := strings.CutPrefix(*ciphertext, prefix)
	if !ok {
		return nil, errors.New("unsupported ciphertext version")
	}
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(data) < 12+16 {
		return nil, errors.New("invalid ciphertext")
	}
	return &Value{Nonce: data[:12], Ciphertext: data[12:]}, nil
}
