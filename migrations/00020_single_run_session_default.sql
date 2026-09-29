-- +goose Up
ALTER TABLE sessions ALTER COLUMN allow_multiple_runs SET NOT NULL;
ALTER TABLE sessions ALTER COLUMN allow_multiple_runs SET DEFAULT false;

-- +goose Down
ALTER TABLE sessions ALTER COLUMN allow_multiple_runs DROP DEFAULT;
ALTER TABLE sessions ALTER COLUMN allow_multiple_runs DROP NOT NULL;
