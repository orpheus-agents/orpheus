-- +goose Up
ALTER TABLE sessions ADD COLUMN allow_multiple_runs boolean;
ALTER TABLE sessions DROP CONSTRAINT sessions_sandbox_state_check;
ALTER TABLE sessions ADD CONSTRAINT sessions_sandbox_state_check
CHECK (sandbox_state IN ('not_created','provisioning','ready','pausing','paused','resuming','unavailable','deleting','deleted'));

-- +goose Down
ALTER TABLE sessions DROP CONSTRAINT sessions_sandbox_state_check;
ALTER TABLE sessions ADD CONSTRAINT sessions_sandbox_state_check
CHECK (sandbox_state IN ('not_created','provisioning','ready','pausing','paused','resuming','unavailable'));
ALTER TABLE sessions DROP COLUMN allow_multiple_runs;
