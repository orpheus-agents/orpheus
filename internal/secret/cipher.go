// Package secret encrypts explicit sandbox environment values at rest.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"

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
