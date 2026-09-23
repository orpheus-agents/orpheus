-- +goose Up
UPDATE sessions SET configuration = jsonb_set(configuration, '{public,limits,max_session_tokens}', '100000000');
UPDATE session_events SET data = data || '{"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}'
WHERE type = 'run.updated';

-- +goose Down
UPDATE sessions SET configuration = configuration #- '{public,limits,max_session_tokens}';
UPDATE runs SET stop_reason = NULL WHERE stop_reason = 'token_limit';
UPDATE session_events SET data = CASE WHEN data->>'stop_reason' = 'token_limit'
  THEN jsonb_set(data - 'usage', '{stop_reason}', 'null') ELSE data - 'usage' END
WHERE type = 'run.updated';
