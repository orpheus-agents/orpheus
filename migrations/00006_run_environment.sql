-- +goose Up
ALTER TABLE runs
  ADD COLUMN env_ciphertext text,
  ADD COLUMN env_names text[] NOT NULL DEFAULT '{}',
  ADD COLUMN env_from text[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE runs
  DROP COLUMN env_ciphertext,
  DROP COLUMN env_names,
  DROP COLUMN env_from;
