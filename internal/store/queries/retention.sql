-- name: SandboxCleanupCandidates :many
WITH candidates AS (
 SELECT s.id,
   CASE WHEN s.sandbox_state = 'paused'
     THEN r.finished_at - sqlc.arg(cutoff)::timestamptz
     ELSE COALESCE(o.next_check_at, o.created_at) - sqlc.arg(now_at)::timestamptz
   END AS overdue
 FROM sessions s
 CROSS JOIN LATERAL (
   SELECT status, finished_at FROM runs WHERE session_id = s.id
   ORDER BY number DESC LIMIT 1
 ) r
 LEFT JOIN LATERAL (
   SELECT status, next_check_at, created_at FROM operations
   WHERE session_id = s.id AND kind = 'sandbox_delete'
     AND parameters->>'reason' = 'sandbox_expired'
     AND parameters->>'sandbox_id' = s.sandbox_id
   ORDER BY created_at DESC, id DESC LIMIT 1
 ) o ON true
 WHERE s.allow_multiple_runs AND NOT s.slot_reserved AND s.sandbox_id IS NOT NULL
   AND NOT EXISTS (
     SELECT 1 FROM operations c WHERE c.session_id = s.id AND c.kind = 'sandbox_create'
       AND c.status IN ('sending', 'uncertain', 'confirmed')
       AND COALESCE(c.result->>'adopted', 'false') <> 'true'
       AND NOT EXISTS (
         SELECT 1 FROM operations d WHERE d.session_id = s.id
           AND d.kind = 'sandbox_delete' AND d.status = 'confirmed'
           AND d.parameters->>'create_operation_id' = c.id::text
           AND d.parameters->>'sandbox_id' = c.result->>'sandbox_id'
       )
   )
   AND r.status IN ('completed', 'failed', 'cancelled')
   AND (
     (s.sandbox_state = 'paused' AND r.finished_at < sqlc.arg(cutoff)::timestamptz)
     OR (s.sandbox_state = 'deleting' AND o.status <> 'confirmed'
       AND COALESCE(o.next_check_at, o.created_at) <= sqlc.arg(now_at)::timestamptz)
   )
)
SELECT id FROM candidates ORDER BY overdue, id LIMIT sqlc.arg(batch_size)::int;

-- name: LastSessionRun :one
SELECT * FROM runs WHERE session_id = $1 ORDER BY number DESC LIMIT 1;

-- name: SandboxRetentionOperation :one
SELECT * FROM operations
WHERE session_id = $1 AND kind = 'sandbox_delete'
  AND parameters->>'reason' = 'sandbox_expired'
ORDER BY created_at DESC, id DESC LIMIT 1;
