-- name: ListSessions :many
SELECT s.* FROM sessions s
JOIN runs latest ON latest.session_id = s.id
WHERE (sqlc.narg(namespace)::text IS NULL OR s.namespace = sqlc.narg(namespace)::text) AND (sqlc.narg(external_key)::text IS NULL OR s.external_key = sqlc.narg(external_key)::text)
AND (sqlc.narg(status)::text IS NULL OR latest.status = sqlc.narg(status)::text)
AND NOT EXISTS (SELECT 1 FROM runs newer WHERE newer.session_id = latest.session_id AND newer.number > latest.number)
AND (sqlc.arg(activity)::text = 'all' OR (latest.status IN ('accepted','starting','running','cancelling','finalizing')) = (sqlc.arg(activity)::text = 'active'))
AND (sqlc.narg(last_run_created_from)::timestamptz IS NULL OR latest.created_at >= sqlc.narg(last_run_created_from)::timestamptz)
AND (sqlc.narg(last_run_created_to)::timestamptz IS NULL OR latest.created_at < sqlc.narg(last_run_created_to)::timestamptz)
AND (s.created_at, s.id) > (CASE WHEN sqlc.arg(first_page)::boolean THEN '-infinity'::timestamptz ELSE sqlc.arg(after_created_at)::timestamptz END, sqlc.arg(after_id)::uuid)
ORDER BY s.created_at ASC, s.id ASC
LIMIT sqlc.arg(page_limit)::int;

-- name: ListSessionsDesc :many
SELECT s.* FROM sessions s
JOIN runs latest ON latest.session_id = s.id
WHERE (sqlc.narg(namespace)::text IS NULL OR s.namespace = sqlc.narg(namespace)::text) AND (sqlc.narg(external_key)::text IS NULL OR s.external_key = sqlc.narg(external_key)::text)
AND (sqlc.narg(status)::text IS NULL OR latest.status = sqlc.narg(status)::text)
AND NOT EXISTS (SELECT 1 FROM runs newer WHERE newer.session_id = latest.session_id AND newer.number > latest.number)
AND (sqlc.arg(activity)::text = 'all' OR (latest.status IN ('accepted','starting','running','cancelling','finalizing')) = (sqlc.arg(activity)::text = 'active'))
AND (sqlc.narg(last_run_created_from)::timestamptz IS NULL OR latest.created_at >= sqlc.narg(last_run_created_from)::timestamptz)
AND (sqlc.narg(last_run_created_to)::timestamptz IS NULL OR latest.created_at < sqlc.narg(last_run_created_to)::timestamptz)
AND (s.created_at, s.id) < (CASE WHEN sqlc.arg(first_page)::boolean THEN 'infinity'::timestamptz ELSE sqlc.arg(after_created_at)::timestamptz END, sqlc.arg(after_id)::uuid)
ORDER BY s.created_at DESC, s.id DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: ListSessionsByLatest :many
SELECT s.* FROM runs latest JOIN sessions s ON s.id = latest.session_id
WHERE NOT EXISTS (SELECT 1 FROM runs newer WHERE newer.session_id = latest.session_id AND newer.number > latest.number)
AND (sqlc.narg(namespace)::text IS NULL OR s.namespace = sqlc.narg(namespace)::text) AND (sqlc.narg(external_key)::text IS NULL OR s.external_key = sqlc.narg(external_key)::text)
AND (sqlc.narg(status)::text IS NULL OR latest.status = sqlc.narg(status)::text)
AND (sqlc.arg(activity)::text = 'all' OR (latest.status IN ('accepted','starting','running','cancelling','finalizing')) = (sqlc.arg(activity)::text = 'active'))
AND (sqlc.narg(last_run_created_from)::timestamptz IS NULL OR latest.created_at >= sqlc.narg(last_run_created_from)::timestamptz)
AND (sqlc.narg(last_run_created_to)::timestamptz IS NULL OR latest.created_at < sqlc.narg(last_run_created_to)::timestamptz)
AND (latest.created_at, s.id) > (CASE WHEN sqlc.arg(first_page)::boolean THEN '-infinity'::timestamptz ELSE sqlc.arg(after_created_at)::timestamptz END, sqlc.arg(after_id)::uuid)
ORDER BY latest.created_at ASC, s.id ASC
LIMIT sqlc.arg(page_limit)::int;

-- name: ListSessionsByLatestDesc :many
SELECT s.* FROM runs latest JOIN sessions s ON s.id = latest.session_id
WHERE NOT EXISTS (SELECT 1 FROM runs newer WHERE newer.session_id = latest.session_id AND newer.number > latest.number)
AND (sqlc.narg(namespace)::text IS NULL OR s.namespace = sqlc.narg(namespace)::text) AND (sqlc.narg(external_key)::text IS NULL OR s.external_key = sqlc.narg(external_key)::text)
AND (sqlc.narg(status)::text IS NULL OR latest.status = sqlc.narg(status)::text)
AND (sqlc.arg(activity)::text = 'all' OR (latest.status IN ('accepted','starting','running','cancelling','finalizing')) = (sqlc.arg(activity)::text = 'active'))
AND (sqlc.narg(last_run_created_from)::timestamptz IS NULL OR latest.created_at >= sqlc.narg(last_run_created_from)::timestamptz)
AND (sqlc.narg(last_run_created_to)::timestamptz IS NULL OR latest.created_at < sqlc.narg(last_run_created_to)::timestamptz)
AND (latest.created_at, s.id) < (CASE WHEN sqlc.arg(first_page)::boolean THEN 'infinity'::timestamptz ELSE sqlc.arg(after_created_at)::timestamptz END, sqlc.arg(after_id)::uuid)
ORDER BY latest.created_at DESC, s.id DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: ListRuns :many
SELECT r.* FROM runs r WHERE r.session_id = sqlc.arg(session_id)
AND (sqlc.narg(input_fingerprint)::text IS NULL OR r.input_fingerprint = sqlc.narg(input_fingerprint)::text) AND (sqlc.narg(status)::text IS NULL OR r.status = sqlc.narg(status)::text)
AND (sqlc.arg(first_page)::boolean OR (r.number) > (sqlc.arg(after_number)::int))
ORDER BY r.number ASC
LIMIT sqlc.arg(page_limit)::int;

-- name: ListRunsDesc :many
SELECT r.* FROM runs r WHERE r.session_id = sqlc.arg(session_id)
AND (sqlc.narg(input_fingerprint)::text IS NULL OR r.input_fingerprint = sqlc.narg(input_fingerprint)::text) AND (sqlc.narg(status)::text IS NULL OR r.status = sqlc.narg(status)::text)
AND (sqlc.arg(first_page)::boolean OR (r.number) < (sqlc.arg(after_number)::int))
ORDER BY r.number DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: ListAllRuns :many
SELECT r.* FROM runs r JOIN sessions s ON s.id=r.session_id
WHERE (sqlc.narg(namespace)::text IS NULL OR s.namespace = sqlc.narg(namespace)::text) AND (sqlc.narg(external_key)::text IS NULL OR s.external_key = sqlc.narg(external_key)::text)
AND (sqlc.narg(input_fingerprint)::text IS NULL OR r.input_fingerprint = sqlc.narg(input_fingerprint)::text) AND (sqlc.narg(status)::text IS NULL OR r.status = sqlc.narg(status)::text)
AND (r.created_at, r.id) > (CASE WHEN sqlc.arg(first_page)::boolean THEN '-infinity'::timestamptz ELSE sqlc.arg(after_created_at)::timestamptz END, sqlc.arg(after_id)::uuid)
ORDER BY r.created_at ASC, r.id ASC
LIMIT sqlc.arg(page_limit)::int;

-- name: ListAllRunsDesc :many
SELECT r.* FROM runs r JOIN sessions s ON s.id=r.session_id
WHERE (sqlc.narg(namespace)::text IS NULL OR s.namespace = sqlc.narg(namespace)::text) AND (sqlc.narg(external_key)::text IS NULL OR s.external_key = sqlc.narg(external_key)::text)
AND (sqlc.narg(input_fingerprint)::text IS NULL OR r.input_fingerprint = sqlc.narg(input_fingerprint)::text) AND (sqlc.narg(status)::text IS NULL OR r.status = sqlc.narg(status)::text)
AND (r.created_at, r.id) < (CASE WHEN sqlc.arg(first_page)::boolean THEN 'infinity'::timestamptz ELSE sqlc.arg(after_created_at)::timestamptz END, sqlc.arg(after_id)::uuid)
ORDER BY r.created_at DESC, r.id DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: ListEvents :many
SELECT sequence::text, session_id, type, created_at, data FROM session_events WHERE session_id = $1 AND sequence>$2 ORDER BY session_events.sequence LIMIT sqlc.arg(page_limit)::int;

-- name: History :many
WITH objects AS (
   SELECT DISTINCT ON(type, data->>'id') type, data FROM session_events
   WHERE session_events.session_id = sqlc.arg(session_id) AND sequence<=sqlc.arg(watermark)::bigint AND type IN ('message.updated', 'tool_call.updated')
   ORDER BY type, data->>'id', sequence DESC
  ), positioned AS (
   SELECT objects.type, objects.data, runs.number AS rn,
    CASE WHEN objects.type = 'message.updated' AND objects.data->'position' = 'null'::jsonb
       AND input_message.role = 'user' AND input_message.delivery_number <= start_message.delivery_number
      THEN 0 WHEN objects.data->'position' = 'null'::jsonb THEN 1 ELSE 0 END AS unknown,
    COALESCE((objects.data->'position'->>'item_index')::bigint,0)::bigint AS idx,
    (objects.data->>'registered_sequence')::bigint AS seq
   FROM objects JOIN runs ON runs.id = (objects.data->>'run_id')::uuid
   LEFT JOIN messages input_message ON objects.type = 'message.updated' AND input_message.id = (objects.data->>'id')::uuid
   LEFT JOIN LATERAL (
     SELECT m.delivery_number FROM operations o JOIN messages m ON m.id = o.message_id
     WHERE o.session_id = sqlc.arg(session_id) AND o.run_id = runs.id AND o.kind = 'start'
     ORDER BY o.created_at LIMIT 1
   ) start_message ON true
   WHERE (sqlc.narg(run_id)::uuid IS NULL OR runs.id = sqlc.narg(run_id)::uuid)
  ) SELECT type, data, rn, unknown, idx, seq FROM positioned
  WHERE (sqlc.arg(first_page)::boolean OR (rn, unknown, idx, seq)>(sqlc.arg(after_run)::bigint, sqlc.arg(after_unknown)::bigint, sqlc.arg(after_index)::bigint, sqlc.arg(after_sequence)::bigint)) ORDER BY rn, unknown, idx, seq LIMIT sqlc.arg(page_limit)::int;

-- name: HistoryByExternalKey :many
WITH objects AS (
   SELECT e.type, e.data FROM messages m
   CROSS JOIN LATERAL (
     SELECT type, data FROM session_events
     WHERE session_events.session_id = sqlc.arg(session_id) AND type = 'message.updated'
       AND data->>'id' = m.id::text AND sequence <= sqlc.arg(watermark)::bigint
     ORDER BY sequence DESC LIMIT 1
   ) e
   WHERE m.session_id = sqlc.arg(session_id) AND m.role = 'user'
     AND m.external_key = sqlc.arg(message_external_key)::text
  ), positioned AS (
   SELECT objects.type, objects.data, runs.number AS rn,
    CASE WHEN objects.type = 'message.updated' AND objects.data->'position' = 'null'::jsonb
       AND input_message.role = 'user' AND input_message.delivery_number <= start_message.delivery_number
      THEN 0 WHEN objects.data->'position' = 'null'::jsonb THEN 1 ELSE 0 END AS unknown,
    COALESCE((objects.data->'position'->>'item_index')::bigint,0)::bigint AS idx,
    (objects.data->>'registered_sequence')::bigint AS seq
   FROM objects JOIN runs ON runs.id = (objects.data->>'run_id')::uuid
   LEFT JOIN messages input_message ON objects.type = 'message.updated' AND input_message.id = (objects.data->>'id')::uuid
   LEFT JOIN LATERAL (
     SELECT m.delivery_number FROM operations o JOIN messages m ON m.id = o.message_id
     WHERE o.session_id = sqlc.arg(session_id) AND o.run_id = runs.id AND o.kind = 'start'
     ORDER BY o.created_at LIMIT 1
   ) start_message ON true
   WHERE (sqlc.narg(run_id)::uuid IS NULL OR runs.id = sqlc.narg(run_id)::uuid)
  ) SELECT type, data, rn, unknown, idx, seq FROM positioned
  WHERE (sqlc.arg(first_page)::boolean OR (rn, unknown, idx, seq)>(sqlc.arg(after_run)::bigint, sqlc.arg(after_unknown)::bigint, sqlc.arg(after_index)::bigint, sqlc.arg(after_sequence)::bigint)) ORDER BY rn, unknown, idx, seq LIMIT sqlc.arg(page_limit)::int;
