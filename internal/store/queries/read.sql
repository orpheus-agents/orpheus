-- name: ListSessions :many
SELECT s.* FROM sessions s WHERE (sqlc.arg(first_page)::boolean OR (created_at, id)>(sqlc.arg(after_date)::timestamptz, sqlc.arg(after_id)::uuid)) ORDER BY created_at, id LIMIT sqlc.arg(page_limit)::int;

-- name: ListRuns :many
SELECT r.* FROM runs r WHERE session_id = $1 AND number>$2 ORDER BY number LIMIT sqlc.arg(page_limit)::int;

-- name: ListEvents :many
SELECT sequence::text, session_id, type, created_at, data FROM session_events WHERE session_id = $1 AND sequence>$2 ORDER BY sequence LIMIT sqlc.arg(page_limit)::int;

-- name: History :many
WITH objects AS (
   SELECT DISTINCT ON(type, data->>'id') type, data FROM session_events
   WHERE session_events.session_id = sqlc.arg(session_id) AND sequence<=sqlc.arg(watermark)::bigint AND type IN ('message.updated', 'tool_call.updated')
   ORDER BY type, data->>'id', sequence DESC
  ), positioned AS (
   SELECT objects.type, objects.data, runs.number AS rn,
    CASE WHEN objects.data->'position' = 'null'::jsonb THEN 1 ELSE 0 END AS unknown,
    COALESCE((objects.data->'position'->>'item_index')::bigint,0)::bigint AS idx,
    (objects.data->>'registered_sequence')::bigint AS seq
   FROM objects JOIN runs ON runs.id = (objects.data->>'run_id')::uuid
   WHERE (sqlc.narg(run_id)::uuid IS NULL OR runs.id = sqlc.narg(run_id)::uuid)
  ) SELECT type, data, rn, unknown, idx, seq FROM positioned
  WHERE (sqlc.arg(first_page)::boolean OR (rn, unknown, idx, seq)>(sqlc.arg(after_run)::bigint, sqlc.arg(after_unknown)::bigint, sqlc.arg(after_index)::bigint, sqlc.arg(after_sequence)::bigint)) ORDER BY rn, unknown, idx, seq LIMIT sqlc.arg(page_limit)::int;
