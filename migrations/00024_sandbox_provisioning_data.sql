-- +goose Up
-- Only a confirmed creation with evidence of use is adopted. A saved sandbox ID
-- alone is not enough: the old worker saved it before Info/Connect and ready.
UPDATE operations o
SET result = o.result || '{"adopted":true}'::jsonb
FROM sessions s
WHERE o.session_id = s.id AND o.kind = 'sandbox_create' AND o.status = 'confirmed'
  AND o.result->>'sandbox_id' = s.sandbox_id
  AND (s.sandbox_state IN ('ready','paused','pausing','deleting','deleted')
       OR s.sandbox_last_known_state IN ('ready','paused')
       OR s.workspace IS NOT NULL OR s.launch_id IS NOT NULL
       OR EXISTS (SELECT 1 FROM runs r WHERE r.session_id = s.id AND r.execution_started_at IS NOT NULL)
       OR EXISTS (SELECT 1 FROM hook_executions h WHERE h.session_id = s.id AND h.started_at IS NOT NULL));

UPDATE operations o
SET deadline_at = COALESCE(o.attempted_at, o.created_at) + interval '120 seconds'
WHERE o.kind = 'sandbox_create' AND COALESCE(o.result->>'adopted','false') <> 'true'
  AND o.status IN ('pending','sending','uncertain','confirmed');

-- Restore scheduler visibility, including terminal runs. The worker publishes
-- terminal states and handles ambiguous records conservatively under its lock.
UPDATE sessions s SET slot_reserved = true
WHERE EXISTS (SELECT 1 FROM operations o WHERE o.session_id = s.id
              AND o.kind = 'sandbox_create' AND o.deadline_at IS NOT NULL
              AND o.status IN ('sending','uncertain','confirmed'));

-- +goose Down
-- Restored reservations must survive downgrade; releasing them could orphan a
-- resource. Downgrade is for migration testing, not a production binary rollback.
UPDATE operations SET result = result - 'adopted', deadline_at = NULL
WHERE kind = 'sandbox_create';
