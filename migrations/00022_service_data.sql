-- +goose Up
UPDATE sessions SET configuration = jsonb_set(configuration, '{public,sandbox,services}', '[]')
WHERE configuration #> '{public,sandbox}' IS NOT NULL;
UPDATE session_events SET data = jsonb_set(data, '{services}', '[]') WHERE type = 'run.updated';

-- +goose Down
UPDATE sessions SET configuration = configuration #- '{public,sandbox,services}';
UPDATE session_events SET data = data - 'services' WHERE type = 'run.updated';
