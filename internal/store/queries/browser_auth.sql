-- name: InsertBrowserLogin :exec
INSERT INTO browser_login_requests (id, request_id, browser_nonce_hash, return_path, expires_at, previous_session_hash)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: GetBrowserLogin :one
SELECT * FROM browser_login_requests
WHERE id = $1 AND browser_nonce_hash = $2 AND expires_at > clock_timestamp();

-- name: ConsumeBrowserLogin :one
DELETE FROM browser_login_requests
WHERE id = $1 AND browser_nonce_hash = $2 AND expires_at > clock_timestamp()
RETURNING *;

-- name: InsertBrowserSession :exec
INSERT INTO browser_sessions (token_hash, subject, display_name, expires_at)
VALUES ($1, $2, $3, $4);

-- name: GetBrowserSession :one
SELECT * FROM browser_sessions WHERE token_hash = $1 AND expires_at > clock_timestamp();

-- name: DeleteBrowserSession :exec
DELETE FROM browser_sessions WHERE token_hash = $1;

-- name: CleanupBrowserLogins :execrows
DELETE FROM browser_login_requests WHERE id IN (
    SELECT id FROM browser_login_requests WHERE expires_at <= clock_timestamp()
    ORDER BY expires_at LIMIT 1000 FOR UPDATE SKIP LOCKED
);

-- name: CleanupBrowserSessions :execrows
DELETE FROM browser_sessions WHERE token_hash IN (
    SELECT token_hash FROM browser_sessions WHERE expires_at <= clock_timestamp()
    ORDER BY expires_at LIMIT 1000 FOR UPDATE SKIP LOCKED
);
