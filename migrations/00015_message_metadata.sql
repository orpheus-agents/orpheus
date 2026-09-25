-- +goose Up
ALTER TABLE messages ADD COLUMN metadata jsonb;

-- +goose Down
ALTER TABLE messages DROP COLUMN metadata;
