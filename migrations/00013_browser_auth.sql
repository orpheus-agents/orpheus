-- +goose Up
CREATE TABLE browser_login_requests (
    id text PRIMARY KEY,
    request_id text NOT NULL UNIQUE,
    browser_nonce_hash bytea NOT NULL CHECK (octet_length(browser_nonce_hash) = 32),
    return_path text NOT NULL,
    expires_at timestamptz NOT NULL,
    -- A cross-site IdP POST omits the Lax session cookie; capture it on login GET.
    previous_session_hash bytea
        CHECK (previous_session_hash IS NULL OR octet_length(previous_session_hash) = 32)
);
CREATE INDEX browser_login_requests_expiry ON browser_login_requests(expires_at);
CREATE TABLE browser_sessions (
    token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    subject text NOT NULL,
    display_name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);
CREATE INDEX browser_sessions_expiry ON browser_sessions(expires_at);
-- +goose Down
DROP TABLE browser_sessions;
DROP TABLE browser_login_requests;
