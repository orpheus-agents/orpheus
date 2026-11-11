-- +goose Up
CREATE TABLE secrets (
    id uuid PRIMARY KEY,
    nonce bytea NOT NULL CHECK (octet_length(nonce) = 12),
    ciphertext bytea NOT NULL CHECK (octet_length(ciphertext) >= 16),
    created_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE sessions ADD COLUMN env_secret_id uuid REFERENCES secrets(id);
ALTER TABLE runs ADD COLUMN env_secret_id uuid REFERENCES secrets(id);

-- +goose Down
ALTER TABLE runs DROP COLUMN env_secret_id;
ALTER TABLE sessions DROP COLUMN env_secret_id;
DROP TABLE secrets;
