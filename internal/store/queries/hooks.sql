-- name: HookExecutions :many
SELECT * FROM hook_executions WHERE run_id = $1 ORDER BY
  CASE name WHEN 'after_create' THEN 1 WHEN 'before_run' THEN 2 WHEN 'after_run' THEN 3 ELSE 4 END;

-- name: HookExecutionsForRuns :many
SELECT * FROM hook_executions WHERE run_id = ANY(sqlc.arg(run_ids)::uuid[]) ORDER BY run_id,
  CASE name WHEN 'after_create' THEN 1 WHEN 'before_run' THEN 2 WHEN 'after_run' THEN 3 ELSE 4 END;

-- name: GetHookExecution :one
SELECT * FROM hook_executions WHERE run_id = $1 AND name = $2;

-- name: SuccessfulAfterCreate :one
SELECT EXISTS (
  SELECT 1 FROM hook_executions WHERE session_id = $1 AND name = 'after_create' AND status = 'completed'
);

-- name: InsertHookExecution :exec
INSERT INTO hook_executions (id,session_id,run_id,name,status)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (run_id,name) DO NOTHING;

-- name: SaveHookExecution :exec
UPDATE hook_executions SET
  status = $2, started_at = $3, deadline_at = $4, cancel_attempted_at = $5,
  kill_attempted_at = $6, start_attempts = $7,
  finished_at = $8, exit_code = $9, signal = $10, output = $11,
  output_completeness = $12, truncation_reason = $13, error = $14,
  stop_reason = $15
WHERE id = $1;

-- name: SkipPendingHooks :exec
UPDATE hook_executions SET status = 'skipped', finished_at = now()
WHERE run_id = $1 AND status = 'pending';
