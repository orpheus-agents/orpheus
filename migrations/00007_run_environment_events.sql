-- +goose Up
UPDATE session_events
SET data = jsonb_build_object('env_names', '[]'::jsonb, 'env_from', '[]'::jsonb) || data
WHERE type = 'run.updated';

-- +goose Down
UPDATE session_events
SET data = data - 'env_names' - 'env_from'
WHERE type = 'run.updated';
