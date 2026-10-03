-- +goose Up
ALTER TABLE runs ADD COLUMN services jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(services) = 'array');

-- +goose Down
ALTER TABLE runs DROP COLUMN services;
