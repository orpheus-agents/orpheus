package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/secret"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

// ErrEnvironmentUnavailable marks missing or invalid encrypted data, rather
// than a database access failure that a worker should retry.
var ErrEnvironmentUnavailable = errors.New("stored environment is unavailable")

func createSecret(ctx context.Context, q db.DBTX, value *secret.Value) (*uuid.UUID, error) {
	if value == nil {
		return nil, nil
	}
	id := uuid.New()
	if err := db.New(q).CreateSecret(ctx, db.CreateSecretParams{ID: id, Nonce: value.Nonce, Ciphertext: value.Ciphertext}); err != nil {
		return nil, err
	}
	return &id, nil
}

// A nil reference means no explicit environment. Referenced secrets fail closed.
func environmentSecret(ctx context.Context, q db.DBTX, id *uuid.UUID) (*secret.Value, error) {
	if id == nil {
		return nil, nil
	}
	row, err := db.New(q).GetSecret(ctx, *id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.Join(ErrEnvironmentUnavailable, err)
	}
	if err != nil {
		return nil, err
	}
	return &secret.Value{Nonce: row.Nonce, Ciphertext: row.Ciphertext}, nil
}

func (s *Store) SessionEnvironment(ctx context.Context, q db.DBTX, record SessionRecord) (map[string]string, error) {
	value, err := environmentSecret(ctx, q, record.EnvSecretID)
	if err != nil {
		return nil, err
	}
	values, err := s.Cipher.Decrypt(record.ID, value)
	if err != nil {
		return nil, errors.Join(ErrEnvironmentUnavailable, err)
	}
	return values, nil
}

func (s *Store) RunEnvironment(ctx context.Context, q db.DBTX, record RunRecord) (map[string]string, error) {
	value, err := environmentSecret(ctx, q, record.EnvSecretID)
	if err != nil {
		return nil, err
	}
	values, err := s.Cipher.DecryptRun(record.SessionID, record.ID, value)
	if err != nil {
		return nil, errors.Join(ErrEnvironmentUnavailable, err)
	}
	return values, nil
}
