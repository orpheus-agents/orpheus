//go:build integration || live

package testutil

import (
	"testing"

	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/session"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/orpheus-agents/orpheus/internal/secret"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

func SeedProfile(t *testing.T, pool *pgxpool.Pool, cipher *secret.Cipher, name, model, key string) db.ModelProfile {
	t.Helper()
	accountID, secretID := uuid.New(), uuid.New()
	value, err := cipher.EncryptAccount(accountID, key)
	if err != nil {
		t.Fatal(err)
	}
	q := db.New(pool)
	if err := q.CreateSecret(t.Context(), db.CreateSecretParams{ID: secretID, Nonce: value.Nonce, Ciphertext: value.Ciphertext}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateLLMAccount(t.Context(), db.CreateLLMAccountParams{ID: accountID, Name: name, Harness: "codex", AuthMode: "api_key", Active: true, ApiKeySecretID: &secretID}); err != nil {
		t.Fatal(err)
	}
	profile, err := q.CreateModelProfile(t.Context(), db.CreateModelProfileParams{ID: uuid.New(), Name: name, LlmAccountID: accountID, Model: model, Codex: []byte("{}"), Active: true})
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

// SeedFileAccount imports a file source without contacting S3, as the migration does.
func SeedFileAccount(t *testing.T, pool *pgxpool.Pool, storeName, key string, metadata session.CredentialStore, profiles ...string) config.Account {
	t.Helper()
	id := uuid.New()
	q := db.New(pool)
	if _, err := q.CreateLLMAccount(t.Context(), db.CreateLLMAccountParams{ID: id, Name: "File account", Harness: "codex", AuthMode: "account", Active: true, CredentialStore: &storeName, CredentialKey: &key}); err != nil {
		t.Fatal(err)
	}
	for _, name := range profiles {
		if _, err := q.CreateModelProfile(t.Context(), db.CreateModelProfileParams{ID: uuid.New(), Name: name, LlmAccountID: id, Model: "model", Codex: []byte("{}"), Active: true}); err != nil {
			t.Fatal(err)
		}
	}
	fingerprint, err := config.SourceFingerprint("codex", session.Credentials{Mode: "account", Store: &metadata, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	return config.Account{ID: id.String(), Profiles: profiles, Fingerprint: fingerprint}
}
