-- +goose Up
UPDATE session_events SET data = jsonb_build_object('external_key', NULL) || data WHERE type = 'message.updated';
UPDATE session_events SET data = jsonb_build_object('input_fingerprint', NULL) || data WHERE type = 'run.updated';
UPDATE session_events SET data = jsonb_set(data, '{final_message}', jsonb_build_object('external_key', NULL) || (data->'final_message'))
WHERE type = 'run.updated' AND jsonb_typeof(data->'final_message') = 'object';

-- +goose Down
UPDATE session_events SET data = data - 'external_key' WHERE type = 'message.updated';
UPDATE session_events SET data = data - 'input_fingerprint' WHERE type = 'run.updated';
UPDATE session_events SET data = jsonb_set(data, '{final_message}', (data->'final_message') - 'external_key')
WHERE type = 'run.updated' AND jsonb_typeof(data->'final_message') = 'object';
