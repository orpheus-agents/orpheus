-- name: GetSession :one
SELECT s.*
FROM sessions s
WHERE id = $1;

-- name: LockSession :one
SELECT s.*
FROM sessions s
WHERE id = $1 FOR UPDATE;

-- name: GetRun :one
SELECT r.*
FROM runs r
WHERE session_id = $1 AND id = $2;

-- name: ActiveRun :one
SELECT r.*
FROM runs r
WHERE session_id = $1 AND status IN ('accepted', 'starting', 'running', 'cancelling', 'finalizing');

-- name: LatestRun :one
SELECT r.*
FROM runs r
WHERE session_id = $1
ORDER BY number DESC
LIMIT 1;

-- name: AdvisoryLock :exec
SELECT pg_advisory_xact_lock(sqlc.arg(lock_key)::bigint);

-- name: SaveSession :exec
UPDATE sessions
SET sandbox_state = $2,
    sandbox_last_known_state = $3,
    sandbox_error = $4,
    sandbox_id = $5,
    process_id = $6,
    launch_id = $7,
    thread_id = $8,
    history_path = $9,
    history_offset = $10,
    workspace = $11,
    harness_home = $12,
    slot_reserved = $13,
    next_run_number = $14,
    next_event_sequence = $15,
    input_tokens = $16,
    output_tokens = $17,
    total_tokens = $18
WHERE id = $1;

-- name: SaveRun :exec
UPDATE runs
SET status = $2,
    observation = $3,
    execution_started_at = $4,
    deadline_at = $5,
    finished_at = $6,
    cancel_requested_at = $7,
    cancel_attempted_at = $8,
    stop_reason = $9,
    stop_method = $10,
    error = $11,
    native_turn_id = $12,
    next_delivery_number = $13,
    final_message_id = $14,
    phase = $15,
    agent_status = $16,
    agent_error = $17,
    input_tokens = $18,
    output_tokens = $19,
    total_tokens = $20
WHERE id = $1;

-- name: InsertEvent :exec
INSERT INTO session_events(session_id, sequence, type, data)
VALUES($1, $2, $3, $4);

-- name: UpsertMessage :exec
INSERT INTO messages(id, session_id, run_id, role, kind, text, delivery_status, delivery_number, error, native_key, registered_sequence, position, created_at, external_key, metadata)
VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
ON CONFLICT(id) DO UPDATE SET kind = EXCLUDED.kind,
    text = EXCLUDED.text,
    delivery_status = EXCLUDED.delivery_status,
    error = EXCLUDED.error,
    native_key = EXCLUDED.native_key,
    position = EXCLUDED.position;

-- name: UpsertTool :exec
INSERT INTO tool_calls(id, session_id, run_id, name, input, status, result, output_completeness, truncation_reason, native_key, registered_sequence, position, created_at, result_digest)
VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT(id) DO UPDATE SET name = EXCLUDED.name,
    input = EXCLUDED.input,
    status = EXCLUDED.status,
    result = EXCLUDED.result,
    result_digest = EXCLUDED.result_digest,
    output_completeness = EXCLUDED.output_completeness,
    truncation_reason = EXCLUDED.truncation_reason,
    position = EXCLUDED.position;

-- name: GetIdempotency :one
SELECT fingerprint, session_id, run_id, message_id
FROM idempotency_keys
WHERE operation = $1 AND resource = $2 AND key = $3;

-- name: CreateSession :one
INSERT INTO sessions AS s(id, configuration, env_ciphertext, slot_reserved, namespace, external_key)
VALUES($1, $2, $3, false, $4, $5)
RETURNING s.*;

-- name: CountReserved :one
SELECT count(*)
FROM sessions
WHERE slot_reserved;

-- name: CreateRun :one
INSERT INTO runs AS r(id, session_id, number, input_fingerprint, env_ciphertext, env_names, env_from)
VALUES($1, $2, $3, $4, $5, $6, $7)
RETURNING r.*;

-- name: InsertIdempotency :exec
INSERT INTO idempotency_keys(operation, resource, key, fingerprint, session_id, run_id, message_id)
VALUES($1, $2, $3, $4, $5, $6, $7);

-- name: ReservedSessions :many
SELECT id
FROM sessions
WHERE slot_reserved;

-- name: GetMessage :one
SELECT *
FROM messages
WHERE id = $1;

-- name: MessagesByIDs :many
SELECT *
FROM messages
WHERE id = ANY(sqlc.arg(message_ids)::uuid[]);

-- name: Messages :many
SELECT *
FROM messages
WHERE run_id = $1
ORDER BY delivery_number, registered_sequence;

-- name: TryWorkerLock :one
SELECT pg_try_advisory_lock(sqlc.arg(lock_key)::bigint);
