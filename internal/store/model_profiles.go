package store

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orpheus-agents/orpheus/internal/llm"
	"github.com/orpheus-agents/orpheus/internal/store/db"
)

func profileView(row db.GetModelProfileRow) (llm.Profile, error) {
	out := llm.Profile{ID: row.ID, Name: row.Name, Description: row.Description, Harness: row.Harness, AccountID: row.LlmAccountID, AccountName: row.AccountName, AccountActive: row.AccountActive, Active: row.Active, Available: row.Active && row.AccountActive, Model: row.Model, Instructions: row.Instructions}
	err := json.Unmarshal(row.Codex, &out.Codex)
	return out, err
}
func (s *Store) Profile(ctx context.Context, id uuid.UUID) (llm.Profile, error) {
	row, err := db.New(s.Pool).GetModelProfile(ctx, id)
	if err != nil {
		return llm.Profile{}, catalogError(err, "profile")
	}
	return profileView(row)
}
func (s *Store) CreateProfile(ctx context.Context, input llm.ProfileInput) (llm.Profile, error) {
	in, err := llm.NormalizeProfile(input)
	if err != nil {
		return llm.Profile{}, err
	}
	id := uuid.New()
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		account, err := q.ShareLLMAccount(ctx, in.AccountID)
		if err != nil {
			return catalogError(err, "account")
		}
		if account.Harness != "codex" && len(in.Codex) > 0 {
			return llm.Invalid("codex")
		}
		options, err := json.Marshal(in.Codex)
		if err != nil {
			return err
		}
		_, err = q.CreateModelProfile(ctx, db.CreateModelProfileParams{ID: id, Name: in.Name, Description: in.Description, LlmAccountID: in.AccountID, Model: in.Model, Instructions: in.Instructions, Codex: options, Active: *in.Active})
		return err
	})
	if err != nil {
		return llm.Profile{}, catalogError(err, "profile")
	}
	return s.Profile(ctx, id)
}
func (s *Store) UpdateProfile(ctx context.Context, id uuid.UUID, raw []byte) (llm.Profile, error) {
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		old, err := q.LockModelProfile(ctx, id)
		if err != nil {
			return err
		}
		in := llm.ProfileInput{Name: old.Name, Description: old.Description, AccountID: old.LlmAccountID, Active: new(old.Active), Model: old.Model, Instructions: old.Instructions}
		if err = json.Unmarshal(old.Codex, &in.Codex); err != nil {
			return err
		}
		in, err = llm.PatchProfile(in, raw)
		if err != nil {
			return err
		}
		previous, err := q.ReadLLMAccount(ctx, old.LlmAccountID)
		if err != nil {
			return err
		}
		account, err := q.ShareLLMAccount(ctx, in.AccountID)
		if err != nil {
			return catalogError(err, "account")
		}
		if account.Harness != previous.Harness {
			return llm.Invalid("account_id")
		}
		if account.Harness != "codex" && len(in.Codex) > 0 {
			return llm.Invalid("codex")
		}
		options, err := json.Marshal(in.Codex)
		if err != nil {
			return err
		}
		_, err = q.UpdateModelProfile(ctx, db.UpdateModelProfileParams{ID: id, Description: in.Description, LlmAccountID: in.AccountID, Model: in.Model, Instructions: in.Instructions, Codex: options, Active: *in.Active})
		return err
	})
	if err != nil {
		return llm.Profile{}, catalogError(err, "profile")
	}
	return s.Profile(ctx, id)
}
func (s *Store) DeleteProfile(ctx context.Context, id uuid.UUID) error {
	n, err := db.New(s.Pool).DeleteModelProfile(ctx, id)
	if err == nil && n == 0 {
		err = pgx.ErrNoRows
	}
	return catalogError(err, "profile")
}
