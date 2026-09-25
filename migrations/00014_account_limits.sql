-- +goose Up
CREATE TABLE account_limit_observations (
    account_id text NOT NULL,
    source_fingerprint text NOT NULL,
    last_attempt_at timestamptz NOT NULL,
    last_success_at timestamptz,
    last_error_code text,
    snapshot jsonb,
    PRIMARY KEY (account_id, source_fingerprint),
    CHECK (length(account_id) BETWEEN 1 AND 128),
    CHECK (source_fingerprint ~ '^[0-9a-f]{64}$'),
    CHECK (last_error_code IS NULL OR last_error_code IN
        ('unsupported', 'temporarily_unavailable', 'invalid_response', 'no_data', 'authentication_unavailable')),
    CHECK (last_success_at IS NULL OR snapshot IS NOT NULL)
);

-- +goose Down
DROP TABLE account_limit_observations;
