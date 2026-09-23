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

func (c *Cipher) Encrypt(id uuid.UUID, env map[string]string) (*string, error) {
	return c.encrypt("aes-gcm-v1:", id[:], env)
}

func runAAD(sessionID, runID uuid.UUID) []byte {
	aad := []byte("orpheus-run-env-v1\x00")
	aad = append(aad, sessionID[:]...)
	return append(aad, runID[:]...)
}

func (c *Cipher) EncryptRun(sessionID, runID uuid.UUID, env map[string]string) (*string, error) {
	return c.encrypt("aes-gcm-run-v1:", runAAD(sessionID, runID), env)
}

func (c *Cipher) encrypt(prefix string, aad []byte, env map[string]string) (*string, error) {
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
	sealed := c.aead.Seal(nonce, nonce, data, aad)
	return new(prefix + base64.RawURLEncoding.EncodeToString(sealed)), nil
}

func (c *Cipher) Decrypt(id uuid.UUID, ciphertext *string) (map[string]string, error) {
	return c.decrypt("aes-gcm-v1:", id[:], ciphertext)
}

func (c *Cipher) DecryptRun(sessionID, runID uuid.UUID, ciphertext *string) (map[string]string, error) {
	return c.decrypt("aes-gcm-run-v1:", runAAD(sessionID, runID), ciphertext)
}

func (c *Cipher) decrypt(prefix string, aad []byte, ciphertext *string) (map[string]string, error) {
	values := map[string]string{}
	if ciphertext == nil {
		return values, nil
	}
	token, ok := strings.CutPrefix(*ciphertext, prefix)
	if !ok {
		return nil, errors.New("unsupported ciphertext version")
	}
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(data) < c.aead.NonceSize() {
		return nil, errors.New("invalid ciphertext")
	}
	plain, err := c.aead.Open(nil, data[:c.aead.NonceSize()], data[c.aead.NonceSize():], aad)
	if err != nil {
		return nil, errors.New("environment cannot be decrypted")
	}
	if err := json.Unmarshal(plain, &values); err != nil {
		return nil, errors.New("invalid encrypted environment")
	}
	return values, nil
}
