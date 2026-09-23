-- +goose Up
UPDATE session_events
SET data = jsonb_build_object('id', NULL, 'workspace', NULL) || data
WHERE type = 'sandbox.updated';

-- +goose Down
UPDATE session_events
SET data = data - 'id' - 'workspace'
WHERE type = 'sandbox.updated';
