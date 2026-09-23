-- +goose Up
UPDATE sessions SET configuration = jsonb_set(configuration, '{public,hooks}', '{"timeout_seconds":300}'::jsonb)
WHERE NOT (configuration->'public' ? 'hooks');

UPDATE session_events SET data = data || '{"phase":null,"agent_status":null,"agent_error":null,"hooks":[]}'::jsonb
WHERE type = 'run.updated';

-- +goose Down
UPDATE session_events SET data = data - 'phase' - 'agent_status' - 'agent_error' - 'hooks'
WHERE type = 'run.updated';
UPDATE sessions SET configuration = jsonb_set(configuration, '{public}', (configuration->'public') - 'hooks')
WHERE configuration ? 'public';
