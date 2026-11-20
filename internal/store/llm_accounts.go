package store

import (
	"context"
	"errors"
	"time"

	"github.com/aws/smithy-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/orpheus-agents/orpheus/internal/credentials"
	"github.com/orpheus-agents/orpheus/internal/llm"
	"github.com/orpheus-agents/orpheus/internal/secret"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

// ErrCredentialsUnavailable marks absent or invalid authentication data.
// Database access failures remain retryable and are returned unchanged.
var ErrCredentialsUnavailable = errors.New("account credentials are unavailable")

func catalogError(err error, kind string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return session.Problem(404, kind+"_not_found", "Catalog record not found.")
	}
	if failure, ok := errors.AsType[*pgconn.PgError](err); ok {
		switch failure.Code {
		case "23505":
			return session.Problem(409, kind+"_exists", "This catalog name is already registered.")
		case "23503":
			return session.Problem(409, "account_in_use", "The account is referenced by a profile or session.")
		}
	}
	return err
}
func accountView(row db.GetLLMAccountRow) llm.Account {
	return llm.Account{ID: row.ID, Name: row.Name, Harness: row.Harness, Provider: row.Provider, AuthMode: row.AuthMode, Active: row.Active, CredentialStore: row.CredentialStore, Profiles: row.Profiles}
}
func (s *Store) accountKey(ctx context.Context, q db.DBTX, row db.LlmAccount) (string, error) {
	if row.ApiKeySecretID == nil || s.Cipher == nil {
		return "", ErrCredentialsUnavailable
	}
	encrypted, err := db.New(q).GetSecret(ctx, *row.ApiKeySecretID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrCredentialsUnavailable
	}
	if err != nil {
		return "", err
	}
	value, err := s.Cipher.DecryptAccount(row.ID, &secret.Value{Nonce: encrypted.Nonce, Ciphertext: encrypted.Ciphertext})
	if err != nil {
		return "", ErrCredentialsUnavailable
	}
	return value, nil
}
func (s *Store) Account(ctx context.Context, id uuid.UUID) (llm.AccountDetails, error) {
	var out llm.AccountDetails
	err := s.Snapshot(ctx, func(tx pgx.Tx) error {
		row, err := db.New(tx).GetLLMAccount(ctx, id)
		if err != nil {
			return err
		}
		out.Account = accountView(row)
		if row.AuthMode == "api_key" {
			key, err := s.accountKey(ctx, tx, db.LlmAccount{ID: row.ID, ApiKeySecretID: row.ApiKeySecretID})
			if err != nil {
				return err
			}
			out.APIKeyMask = new(llm.MaskKey(key))
		}
		return nil
	})
	return out, catalogError(err, "account")
}
func (s *Store) CreateAccount(ctx context.Context, input llm.AccountInput) (llm.AccountDetails, error) {
	in, err := llm.NormalizeAccount(input)
	if err != nil {
		return llm.AccountDetails{}, err
	}
	id := uuid.New()
	args := db.CreateLLMAccountParams{ID: id, Name: in.Name, Harness: in.Harness, Provider: in.Provider, AuthMode: in.AuthMode, Active: *in.Active, CredentialStore: in.CredentialStore}
	var encrypted *secret.Value
	if in.AuthMode == "account" {
		args.CredentialKey = new("llm-accounts/" + id.String() + "/auth.json")
		if credentials.ValidateFile(in.Harness, in.AuthFile) != nil {
			return llm.AccountDetails{}, llm.Invalid("auth_file")
		}
		fileCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		files, err := s.accountFiles(fileCtx, *in.CredentialStore, *args.CredentialKey)
		if err != nil {
			return llm.AccountDetails{}, err
		}
		if _, err = files.Create(fileCtx, in.AuthFile); err != nil {
			return llm.AccountDetails{}, fileError(err)
		}
	} else {
		if s.Cipher == nil {
			return llm.AccountDetails{}, ErrCredentialsUnavailable
		}
		encrypted, err = s.Cipher.EncryptAccount(id, *in.APIKey)
		if err != nil {
			return llm.AccountDetails{}, err
		}
		args.ApiKeySecretID = new(uuid.New())
	}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		if encrypted != nil {
			if err := q.CreateSecret(ctx, db.CreateSecretParams{ID: *args.ApiKeySecretID, Nonce: encrypted.Nonce, Ciphertext: encrypted.Ciphertext}); err != nil {
				return err
			}
		}
		_, err := q.CreateLLMAccount(ctx, args)
		return err
	})
	if err != nil {
		return llm.AccountDetails{}, catalogError(err, "account")
	}
	return s.Account(ctx, id)
}
func (s *Store) UpdateAccount(ctx context.Context, id uuid.UUID, raw []byte) (llm.AccountDetails, error) {
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := q.LockLLMAccount(ctx, id)
		if err != nil {
			return err
		}
		in, err := llm.PatchAccount(llm.Account{Name: row.Name, Active: row.Active}, raw)
		if err != nil {
			return err
		}
		_, err = q.UpdateLLMAccount(ctx, db.UpdateLLMAccountParams{ID: id, Name: in.Name, Active: in.Active})
		return err
	})
	if err != nil {
		return llm.AccountDetails{}, catalogError(err, "account")
	}
	return s.Account(ctx, id)
}
func (s *Store) DeleteAccount(ctx context.Context, id uuid.UUID) error {
	return catalogError(pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := q.LockLLMAccount(ctx, id)
		if err != nil {
			return err
		}
		n, err := q.DeleteLLMAccount(ctx, id)
		if err != nil {
			return err
		}
		if n == 0 {
			return session.Problem(409, "account_in_use", "The account is referenced by a profile or session.")
		}
		if row.ApiKeySecretID != nil {
			_, err = q.DeleteSecret(ctx, *row.ApiKeySecretID)
		}
		return err
	}), "account")
}
func (s *Store) ReplaceAccountKey(ctx context.Context, id uuid.UUID, key string) error {
	if !llm.ValidKey(key) {
		return llm.Invalid("api_key")
	}
	return catalogError(pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := q.LockLLMAccount(ctx, id)
		if err != nil {
			return err
		}
		if row.AuthMode != "api_key" {
			return llm.Invalid("auth_mode")
		}
		if s.Cipher == nil {
			return ErrCredentialsUnavailable
		}
		encrypted, err := s.Cipher.EncryptAccount(id, key)
		if err != nil {
			return err
		}
		n, err := q.UpdateSecret(ctx, db.UpdateSecretParams{ID: *row.ApiKeySecretID, Nonce: encrypted.Nonce, Ciphertext: encrypted.Ciphertext})
		if err == nil && n != 1 {
			return errors.New("account key is unavailable")
		}
		return err
	}), "account")
}
func (s *Store) accountFiles(ctx context.Context, storeName, key string) (credentials.FileStore, error) {
	settings, ok := s.Deployment.CredentialStores[storeName]
	if !ok {
		return nil, llm.Invalid("credential_store")
	}
	files, err := credentials.OpenStore(ctx, settings, key)
	return files, fileError(err)
}
func fileError(err error) error {
	if err == nil {
		return nil
	}
	if failure, ok := errors.AsType[smithy.APIError](err); ok && (failure.ErrorCode() == "PreconditionFailed" || failure.ErrorCode() == "ConditionalRequestConflict") {
		return session.Problem(409, "auth_file_changed", "The authorization file changed. Reload it before saving.")
	}
	return session.Problem(503, "credentials_unavailable", "Account credentials are unavailable.")
}
func (s *Store) ReadAccountFile(ctx context.Context, id uuid.UUID) (credentials.Blob, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	row, err := db.New(s.Pool).ReadLLMAccount(ctx, id)
	if err != nil {
		return credentials.Blob{}, catalogError(err, "account")
	}
	if row.AuthMode != "account" {
		return credentials.Blob{}, llm.Invalid("auth_mode")
	}
	files, err := s.accountFiles(ctx, *row.CredentialStore, *row.CredentialKey)
	if err != nil {
		return credentials.Blob{}, err
	}
	blob, err := files.Get(ctx)
	if err == nil {
		err = credentials.ValidateFile(row.Harness, blob.Raw)
	}
	if err == nil && blob.ETag == "" {
		err = errors.New("missing ETag")
	}
	return blob, fileError(err)
}
func (s *Store) ReplaceAccountFile(ctx context.Context, id uuid.UUID, raw []byte, etag string) (string, error) {
	if etag == "" || etag == "*" {
		return "", llm.Invalid("If-Match")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	row, err := db.New(s.Pool).ReadLLMAccount(ctx, id)
	if err != nil {
		return "", catalogError(err, "account")
	}
	if row.AuthMode != "account" {
		return "", llm.Invalid("auth_mode")
	}
	if credentials.ValidateFile(row.Harness, raw) != nil {
		return "", llm.Invalid("auth_file")
	}
	files, err := s.accountFiles(ctx, *row.CredentialStore, *row.CredentialKey)
	if err != nil {
		return "", err
	}
	version, err := files.Put(ctx, raw, etag)
	return version, fileError(err)
}
