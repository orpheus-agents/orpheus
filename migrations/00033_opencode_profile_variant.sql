-- +goose Up
ALTER TABLE model_profiles ADD COLUMN opencode_variant text
    CHECK (char_length(opencode_variant) BETWEEN 1 AND 255 AND btrim(opencode_variant) = opencode_variant);

-- +goose Down
ALTER TABLE model_profiles DROP COLUMN opencode_variant;
