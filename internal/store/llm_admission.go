package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/llm"
	"github.com/orpheus-agents/orpheus/internal/session"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

func profileUnavailable(code string) error {
	err := session.Problem(422, "unknown_profile", "The selected profile is unavailable.")
	err.Problem.Details = []session.Detail{{Path: []any{"body", "configuration", "agent", "profile"}, Code: code}}
	return err
}
func (s *Store) resolveProfile(ctx context.Context, tx pgx.Tx, in session.ConfigurationInput) (session.ResolvedConfiguration, uuid.UUID, error) {
	var empty session.ResolvedConfiguration
	q := db.New(tx)
	profile, err := q.ModelProfileForAdmission(ctx, in.Agent.Profile)
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, uuid.Nil, profileUnavailable("profile_not_found")
	}
	if err != nil {
		return empty, uuid.Nil, err
	}
	if !profile.Active {
		return empty, uuid.Nil, profileUnavailable("profile_inactive")
	}
	account, err := q.ShareLLMAccount(ctx, profile.LlmAccountID)
	if err != nil {
		return empty, uuid.Nil, err
	}
	if !account.Active {
		return empty, uuid.Nil, profileUnavailable("account_inactive")
	}
	input := llm.ProfileInput{Name: profile.Name, AccountID: profile.LlmAccountID, Model: profile.Model, Instructions: profile.Instructions}
	if err = json.Unmarshal(profile.Codex, &input.Codex); err != nil {
		return empty, uuid.Nil, err
	}
	snapshot, err := config.Resolve(in, input, account.Harness, s.Deployment, s.Settings.HarnessEnvAllowlist, s.Settings.DefaultMaxSessionTokens)
	return snapshot, account.ID, err
}

func (s *Store) credentialSource(row db.LlmAccount) (session.Credentials, error) {
	out := session.Credentials{AccountID: row.ID.String(), Mode: row.AuthMode}
	if row.AuthMode == "account" {
		if row.CredentialStore == nil || row.CredentialKey == nil {
			return out, ErrCredentialsUnavailable
		}
		settings, ok := s.Deployment.CredentialStores[*row.CredentialStore]
		if !ok {
			return out, ErrCredentialsUnavailable
		}
		out.Store = &settings
		out.Key = *row.CredentialKey
	}
	return out, nil
}
func (s *Store) SessionCredentials(ctx context.Context, record SessionRecord) (session.Credentials, error) {
	if record.LlmAccountID == nil {
		return session.Credentials{}, ErrCredentialsUnavailable
	}
	row, err := db.New(s.Pool).ReadLLMAccount(ctx, *record.LlmAccountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return session.Credentials{}, ErrCredentialsUnavailable
	}
	if err != nil {
		return session.Credentials{}, err
	}
	source, err := s.credentialSource(row)
	if err == nil && row.AuthMode == "api_key" {
		source.APIKey, err = s.accountKey(ctx, s.Pool, row)
	}
	return source, err
}
func (s *Store) ValidateCredentialStores(ctx context.Context) error {
	names, err := db.New(s.Pool).AccountStoreNames(ctx)
	if err != nil {
		return err
	}
	for _, name := range names {
		if _, ok := s.Deployment.CredentialStores[name]; !ok {
			return errors.New("configured credential store is missing")
		}
	}
	return nil
}
