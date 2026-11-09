package store

import (
	"context"
	"errors"
	"maps"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

func (s *Store) replay(ctx context.Context, q db.DBTX, a Admission, op, resource, digest string) (session.Acceptance, bool, error) {
	prior, err := db.New(q).GetIdempotency(ctx, db.GetIdempotencyParams{Operation: op, Resource: resource, Key: a.Key})
	if err == nil {
		result := session.Acceptance{SessionID: prior.SessionID, RunID: prior.RunID, MessageID: prior.MessageID}
		same := prior.Fingerprint == digest
		if same && a.Create != nil {
			r, e := GetSession(ctx, q, result.SessionID, false)
			if e != nil {
				return session.Acceptance{}, false, e
			}
			env, e := s.Cipher.Decrypt(r.ID, r.EnvCiphertext)
			if e != nil {
				return session.Acceptance{}, false, session.Problem(503, "storage_unavailable", "Stored environment cannot be decrypted.")
			}
			same = maps.Equal(env, a.Create.Configuration.Sandbox.Env)
		}
		if same && a.RunID == uuid.Nil {
			r, e := GetRun(ctx, q, result.SessionID, result.RunID)
			if e != nil {
				return session.Acceptance{}, false, e
			}
			env, e := s.Cipher.DecryptRun(r.SessionID, r.ID, r.EnvCiphertext)
			if e != nil {
				return session.Acceptance{}, false, session.Problem(503, "storage_unavailable", "Stored run environment cannot be decrypted.")
			}
			same = maps.Equal(env, a.Env)
		}
		if !same {
			return session.Acceptance{}, false, session.Problem(409, "idempotency_conflict", "Idempotency key was used for another request.")
		}
		return result, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return session.Acceptance{}, false, err
	}

	return session.Acceptance{}, false, nil
}
