-- name: FindOperation :one
SELECT o.*
FROM operations o
WHERE session_id = $1 AND kind = $2 AND run_id IS NOT DISTINCT
FROM sqlc.narg(run_id)::uuid AND message_id IS NOT DISTINCT
FROM sqlc.narg(message_id)::uuid
ORDER BY created_at DESC
LIMIT 1;

-- name: CreateOperation :one
INSERT INTO operations AS o(id, session_id, run_id, message_id, kind, parameters)
VALUES($1, $2, $3, $4, $5, $6)
RETURNING o.*;

-- name: SaveOperation :exec
UPDATE operations
SET status = $2,
    result = $3,
    attempted_at = $4
WHERE id = $1;

-- name: GetSessionOperation :one
SELECT o.*
FROM operations o
WHERE id = $1 AND session_id = $2;

-- name: GetOperation :one
SELECT o.*
FROM operations o
WHERE id = $1;

-- name: LatestAttempt :one
SELECT o.*
FROM operations o
WHERE session_id = $1 AND kind = $2 AND status IN ('sending', 'uncertain', 'confirmed')
ORDER BY created_at DESC
LIMIT 1;

-- name: LastTerminalRun :one
SELECT r.*
FROM runs r
WHERE session_id = $1 AND status IN ('completed', 'failed', 'cancelled')
ORDER BY number DESC
LIMIT 1;

-- name: PreviousExecutedRun :one
SELECT r.*
FROM runs r
WHERE session_id = $1 AND number<$2 AND (execution_started_at IS NOT NULL OR error->>'code' = 'authentication_failed')
ORDER BY number DESC
LIMIT 1;
