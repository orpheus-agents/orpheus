-- name: ReconcileRuns :many
SELECT r.*
FROM runs r
WHERE session_id = $1 AND (status IN ('accepted', 'starting', 'running', 'cancelling') OR native_turn_id = ANY(sqlc.arg(turn_ids)::text[]))
ORDER BY number;

-- name: ToolMetadata :many
SELECT id, session_id, run_id, name, input, status, output_completeness, truncation_reason, native_key, registered_sequence, position, created_at,(result IS NOT NULL)::boolean AS has_result, result_digest
FROM tool_calls
WHERE session_id = $1 AND native_key = ANY(sqlc.arg(native_keys)::text[]);

-- name: ReconcileOperations :many
SELECT o.*
FROM operations o
WHERE session_id = $1 AND kind IN ('start', 'steer') AND run_id = ANY(sqlc.arg(run_ids)::uuid[])
ORDER BY created_at;

-- name: ReconcileMessages :many
SELECT *
FROM messages
WHERE session_id = $1 AND (native_key = ANY(sqlc.arg(native_keys)::text[]) OR native_key IS NULL)
ORDER BY delivery_number, registered_sequence;

-- name: ToolsByID :many
SELECT t.*
FROM tool_calls t
WHERE id = ANY(sqlc.arg(ids)::uuid[]);
