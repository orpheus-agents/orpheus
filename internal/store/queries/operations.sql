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
    attempted_at = $4,
    deadline_at = $5,
    next_check_at = $6,
    check_attempts = $7,
    first_error = $8,
    abandoned_at = $9
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

-- name: OpenSandboxCreate :one
SELECT o.* FROM operations o
WHERE o.session_id = $1 AND o.kind = 'sandbox_create'
  AND o.status IN ('sending','uncertain','confirmed')
  AND COALESCE(o.result->>'adopted','false') <> 'true'
  AND NOT EXISTS (
    SELECT 1 FROM operations d WHERE d.session_id = o.session_id
      AND d.kind = 'sandbox_delete' AND d.status = 'confirmed'
      AND d.parameters->>'create_operation_id' = o.id::text
      AND d.parameters->>'sandbox_id' = o.result->>'sandbox_id')
ORDER BY o.created_at DESC LIMIT 1;

-- name: CurrentSandboxCreate :one
SELECT o.* FROM operations o JOIN runs r ON r.session_id = o.session_id
WHERE o.session_id = $1 AND r.id = $2 AND o.kind = 'sandbox_create'
  AND (o.run_id = r.id OR (o.run_id IS NULL AND o.created_at >= r.created_at
       AND (r.finished_at IS NULL OR o.created_at <= r.finished_at)))
ORDER BY o.created_at DESC LIMIT 1;

-- name: SandboxCreateOwner :many
SELECT r.* FROM runs r JOIN operations o ON o.session_id = r.session_id
WHERE o.id = $1 AND (r.id = o.run_id OR (o.run_id IS NULL
  AND o.created_at >= r.created_at AND (r.finished_at IS NULL OR o.created_at <= r.finished_at)));

-- name: ProvisionHasWork :one
SELECT EXISTS (SELECT 1 FROM hook_executions h WHERE h.session_id = $1 AND h.started_at IS NOT NULL)
    OR EXISTS (SELECT 1 FROM operations o WHERE o.session_id = $1
        AND o.kind IN ('launch','start') AND o.status IN ('sending','uncertain','confirmed')) AS started;

-- name: RestoreProvisionReservations :exec
UPDATE sessions s SET slot_reserved = true
WHERE NOT s.slot_reserved AND EXISTS (
  SELECT 1 FROM operations o WHERE o.session_id = s.id AND o.kind = 'sandbox_create'
    AND o.status IN ('sending','uncertain','confirmed')
    AND COALESCE(o.result->>'adopted','false') <> 'true'
    AND NOT EXISTS (SELECT 1 FROM operations d WHERE d.session_id = s.id
      AND d.kind = 'sandbox_delete' AND d.status = 'confirmed'
      AND d.parameters->>'create_operation_id' = o.id::text
      AND d.parameters->>'sandbox_id' = o.result->>'sandbox_id'));
