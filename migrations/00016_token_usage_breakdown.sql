-- +goose Up
ALTER TABLE sessions
  ADD COLUMN cached_input_tokens bigint NOT NULL DEFAULT 0 CHECK (cached_input_tokens >= 0),
  ADD COLUMN reasoning_output_tokens bigint NOT NULL DEFAULT 0 CHECK (reasoning_output_tokens >= 0),
  ADD COLUMN cached_input_native_total bigint CHECK (cached_input_native_total >= 0),
  ADD COLUMN reasoning_output_native_total bigint CHECK (reasoning_output_native_total >= 0);
ALTER TABLE sessions
  ALTER COLUMN cached_input_native_total SET DEFAULT 0,
  ALTER COLUMN reasoning_output_native_total SET DEFAULT 0;
ALTER TABLE runs
  ADD COLUMN cached_input_tokens bigint NOT NULL DEFAULT 0 CHECK (cached_input_tokens >= 0),
  ADD COLUMN reasoning_output_tokens bigint NOT NULL DEFAULT 0 CHECK (reasoning_output_tokens >= 0);

-- +goose Down
ALTER TABLE runs DROP COLUMN cached_input_tokens, DROP COLUMN reasoning_output_tokens;
ALTER TABLE sessions
  DROP COLUMN cached_input_tokens,
  DROP COLUMN reasoning_output_tokens,
  DROP COLUMN cached_input_native_total,
  DROP COLUMN reasoning_output_native_total;
