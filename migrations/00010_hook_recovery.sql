-- +goose Up
ALTER TABLE hook_executions
  ADD COLUMN kill_attempted_at timestamptz,
  ADD COLUMN start_attempts integer NOT NULL DEFAULT 0 CHECK (start_attempts >= 0);

-- +goose Down
ALTER TABLE hook_executions
  DROP COLUMN kill_attempted_at,
  DROP COLUMN start_attempts;
