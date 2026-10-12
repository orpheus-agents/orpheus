-- +goose Up
ALTER TABLE operations
    ADD COLUMN deadline_at timestamptz,
    ADD COLUMN next_check_at timestamptz,
    ADD COLUMN check_attempts integer NOT NULL DEFAULT 0,
    ADD COLUMN first_error jsonb,
    ADD COLUMN abandoned_at timestamptz;

-- +goose Down
ALTER TABLE operations
    DROP COLUMN deadline_at,
    DROP COLUMN next_check_at,
    DROP COLUMN check_attempts,
    DROP COLUMN first_error,
    DROP COLUMN abandoned_at;
