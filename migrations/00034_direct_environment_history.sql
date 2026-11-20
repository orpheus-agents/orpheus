-- +goose Up
ALTER TABLE runs ADD COLUMN harness_launch_id uuid REFERENCES operations(id);
ALTER TABLE hook_executions
    ADD COLUMN attempt_env_names jsonb NOT NULL DEFAULT '[]',
    ADD COLUMN started_attempt integer;

-- +goose Down
ALTER TABLE hook_executions DROP COLUMN started_attempt, DROP COLUMN attempt_env_names;
ALTER TABLE runs DROP COLUMN harness_launch_id;
