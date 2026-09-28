-- +goose Up
UPDATE session_events
SET data = jsonb_set(data, '{usage}', data->'usage' || '{"cached_input_tokens":0,"reasoning_output_tokens":0}'::jsonb)
WHERE type = 'run.updated';

-- +goose Down
UPDATE session_events
SET data = jsonb_set(data, '{usage}', (data->'usage') - 'cached_input_tokens' - 'reasoning_output_tokens')
WHERE type = 'run.updated';
