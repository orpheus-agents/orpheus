-- +goose Up
CREATE TABLE run_service_credentials (
    run_id uuid PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    invocation_id uuid NOT NULL UNIQUE,
    access_context jsonb NOT NULL,
    scopes jsonb NOT NULL,
    ciphertext text NOT NULL
);
CREATE TABLE run_input_subjects (
    message_id uuid PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
    subject jsonb NOT NULL
);

-- +goose Down
DROP TABLE run_input_subjects;
DROP TABLE run_service_credentials;
