package store

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/servicecredentials"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

// The provider call deliberately runs outside admission's transaction and locks.
func (s *Store) issueCredentials(ctx context.Context, a Admission) (servicecredentials.State, error) {
	state := servicecredentials.State{Entries: map[string]servicecredentials.Entry{}}
	if a.RunID != uuid.Nil {
		return state, nil
	}
	var codes []string
	if a.Create != nil {
		codes = a.Create.Configuration.Sandbox.Services
	} else {
		record, err := GetSession(ctx, s.Pool, a.SessionID, false)
		if err != nil {
			return state, err
		}
		for _, service := range record.Configuration.Public.Sandbox.Services {
			codes = append(codes, service.Code)
		}
	}
	for _, code := range a.Services {
		if service := s.Profiles.Services[code]; service.Provider != nil {
			return state, session.Problem(422, "invalid_service_scope", "File credentials must be selected in configuration.sandbox.services.")
		}
	}
	paths := map[string]bool{}
	for _, code := range slices.Sorted(slices.Values(codes)) {
		cfg := s.Profiles.Services[code].Provider
		if cfg == nil {
			continue
		}
		if paths[cfg.TokenFile] {
			return state, session.Problem(422, "invalid_service_scope", "Protected services cannot share a token file.")
		}
		paths[cfg.TokenFile] = true
		if err := a.AccessContext.Validate(); err != nil {
			return state, err
		}
		grant, err := s.CredentialClient.Issue(ctx, *cfg, code, *a.AccessContext)
		if err != nil {
			return state, err
		}
		state.Context = *a.AccessContext
		state.Entries[code] = servicecredentials.Entry{Config: *cfg, Grant: grant}
	}
	return state, nil
}
func (s *Store) checkCredentials(ctx context.Context, tx pgx.Tx, record SessionRecord, a Admission, state servicecredentials.State) error {
	if len(a.InputSubjects) > 0 && len(a.InputSubjects) != len(a.Messages) {
		return session.Problem(422, "invalid_access_context", "Each message needs its confirmed author.")
	}
	if a.RunID != uuid.Nil {
		saved, err := s.credentials(ctx, tx, record.ID, a.RunID)
		if err != nil {
			return err
		}
		if len(saved.Entries) == 0 {
			return nil
		}
		if len(a.InputSubjects) != len(a.Messages) {
			return session.Problem(422, "access_context_required", "Confirmed input authors are required.")
		}
		if saved.Expired(time.Now()) {
			return session.Problem(403, "service_credentials_expired", "Service credentials expired.")
		}
		for _, subject := range a.InputSubjects {
			if !saved.Context.Allows(subject) {
				return session.Problem(403, "input_subject_denied", "Input author is not allowed for this run.")
			}
		}
		return nil
	}
	if state.Expired(time.Now()) {
		return session.Problem(403, "service_credentials_expired", "Service credentials expired before admission.")
	}
	if len(state.Entries) > 0 {
		if err := Advisory(ctx, tx, LockKey("invocation:"+state.Context.InvocationID.String())); err != nil {
			return err
		}
		accepted, err := db.New(tx).InvocationAccepted(ctx, state.Context.InvocationID)
		if err != nil {
			return err
		}
		if accepted {
			return session.Problem(409, "invocation_already_accepted", "Invocation already has an accepted run. Recover it using the original request key.")
		}
	}
	if a.Create != nil {
		return nil
	}
	last, err := db.New(tx).LatestRun(ctx, record.ID)
	if err != nil {
		return err
	}
	old, err := s.credentials(ctx, tx, record.ID, last.ID)
	if err != nil {
		return err
	}
	compatibleProvider := true
	for code, previous := range old.Entries {
		next, ok := state.Entries[code]
		if !ok || previous.Config.Endpoint != next.Config.Endpoint || previous.Config.SecretEnv != next.Config.SecretEnv || previous.Config.TokenFile != next.Config.TokenFile || !slices.Equal(slices.Sorted(slices.Values(previous.Config.ContextFields)), slices.Sorted(slices.Values(next.Config.ContextFields))) {
			compatibleProvider = false
		}
	}
	if !compatibleProvider || !maps.Equal(old.Scopes(), state.Scopes()) {
		return session.Problem(409, "service_scope_changed", "Service scope changed. Create a clean session using the same invocation.")
	}
	return nil
}
func (s *Store) insertCredentials(ctx context.Context, tx pgx.Tx, sid, rid uuid.UUID, state servicecredentials.State) error {
	if len(state.Entries) == 0 {
		return nil
	}
	token, err := s.sealCredentials(sid, rid, state)
	if err != nil {
		return err
	}
	access, err := json.Marshal(state.Context)
	if err != nil {
		return err
	}
	scopes, err := json.Marshal(state.Scopes())
	if err != nil {
		return err
	}
	return db.New(tx).InsertServiceCredentials(ctx, db.InsertServiceCredentialsParams{RunID: rid, InvocationID: state.Context.InvocationID, AccessContext: access, Scopes: scopes, Ciphertext: *token})
}
func (s *Store) sealCredentials(sid, rid uuid.UUID, state servicecredentials.State) (*string, error) {
	raw, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	return s.Cipher.EncryptCredentials(sid, rid, string(raw))
}
func (s *Store) credentials(ctx context.Context, q db.DBTX, sid, rid uuid.UUID) (servicecredentials.State, error) {
	var state servicecredentials.State
	row, err := db.New(q).GetServiceCredentials(ctx, rid)
	if errors.Is(err, pgx.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	raw, err := s.Cipher.DecryptCredentials(sid, rid, &row.Ciphertext)
	if err != nil {
		return state, session.Problem(503, "storage_unavailable", "Stored service credentials cannot be decrypted.")
	}
	if err = json.Unmarshal([]byte(raw), &state); err != nil || len(state.Entries) == 0 {
		return state, session.Problem(503, "storage_unavailable", "Stored service credentials are invalid.")
	}
	return state, nil
}
func (s *Store) ServiceCredentials(ctx context.Context, sid, rid uuid.UUID) (servicecredentials.State, error) {
	return s.credentials(ctx, s.Pool, sid, rid)
}
func (s *Store) SaveServiceCredentials(ctx context.Context, sid, rid uuid.UUID, state servicecredentials.State) error {
	token, err := s.sealCredentials(sid, rid, state)
	if err != nil {
		return err
	}
	return s.Mutate(ctx, sid, false, func(tx pgx.Tx, _ *SessionRecord) error {
		run, err := GetRun(ctx, tx, sid, rid)
		if err != nil {
			return err
		}
		if run.Status.Terminal() || run.CancelRequestedAt != nil {
			return session.Problem(409, "run_not_accepting_messages", "Run is no longer active.")
		}
		return db.New(tx).UpdateServiceCredentials(ctx, db.UpdateServiceCredentialsParams{RunID: rid, Ciphertext: *token})
	})
}
