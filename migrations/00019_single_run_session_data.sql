-- +goose Up
UPDATE sessions SET allow_multiple_runs = true WHERE allow_multiple_runs IS NULL;

-- +goose Down
-- Deletion cannot be undone. Older workers must see these environments as lost.
UPDATE sessions SET sandbox_state = 'unavailable'
WHERE sandbox_state IN ('deleting','deleted');
UPDATE sessions SET sandbox_last_known_state = 'unavailable'
WHERE sandbox_last_known_state IN ('deleting','deleted');
UPDATE session_events SET data = jsonb_set(data, '{state}', '"unavailable"')
WHERE type = 'sandbox.updated' AND data->>'state' IN ('deleting','deleted');
UPDATE session_events SET data = jsonb_set(data, '{last_known_state}', '"unavailable"')
WHERE type = 'sandbox.updated' AND data->>'last_known_state' IN ('deleting','deleted');
