-- +goose Up
ALTER TABLE sessions
  ADD COLUMN input_tokens bigint NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
  ADD COLUMN output_tokens bigint NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
  ADD COLUMN total_tokens bigint NOT NULL DEFAULT 0 CHECK (total_tokens >= 0);
ALTER TABLE runs
  ADD COLUMN input_tokens bigint NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
  ADD COLUMN output_tokens bigint NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
  ADD COLUMN total_tokens bigint NOT NULL DEFAULT 0 CHECK (total_tokens >= 0);
ALTER TABLE runs DROP CONSTRAINT runs_stop_reason_check;
ALTER TABLE runs ADD CONSTRAINT runs_stop_reason_check CHECK (stop_reason IN ('user_request','run_timeout','token_limit'));

-- +goose Down
ALTER TABLE runs DROP CONSTRAINT runs_stop_reason_check;
ALTER TABLE runs ADD CONSTRAINT runs_stop_reason_check CHECK (stop_reason IN ('user_request','run_timeout'));
ALTER TABLE runs DROP COLUMN input_tokens, DROP COLUMN output_tokens, DROP COLUMN total_tokens;
ALTER TABLE sessions DROP COLUMN input_tokens, DROP COLUMN output_tokens, DROP COLUMN total_tokens;
