-- name: AnalyticsAsOf :one
SELECT transaction_timestamp()::timestamptz AS as_of;

-- name: AnalyticsActiveSessions :one
SELECT COUNT(*)::bigint FROM runs r
JOIN sessions s ON s.id = r.session_id
WHERE r.status IN ('accepted', 'starting', 'running', 'cancelling', 'finalizing')
  AND (sqlc.narg(namespace)::text IS NULL OR s.namespace = sqlc.narg(namespace)::text);
