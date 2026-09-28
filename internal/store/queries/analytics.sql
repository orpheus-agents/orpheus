-- name: AnalyticsAsOf :one
SELECT transaction_timestamp()::timestamptz AS as_of;

-- name: AnalyticsActiveSessions :one
SELECT COUNT(*)::bigint FROM runs r
JOIN sessions s ON s.id = r.session_id
WHERE r.status IN ('accepted', 'starting', 'running', 'cancelling', 'finalizing')
  AND (sqlc.narg(namespace)::text IS NULL OR s.namespace = sqlc.narg(namespace)::text);

-- name: AnalyticsNamespaces :many
SELECT s.namespace,
       COUNT(r.id)::bigint AS runs_count,
       COUNT(r.id) FILTER (WHERE r.status = 'accepted')::bigint AS accepted,
       COUNT(r.id) FILTER (WHERE r.status = 'starting')::bigint AS starting,
       COUNT(r.id) FILTER (WHERE r.status = 'running')::bigint AS running,
       COUNT(r.id) FILTER (WHERE r.status = 'cancelling')::bigint AS cancelling,
       COUNT(r.id) FILTER (WHERE r.status = 'finalizing')::bigint AS finalizing,
       COUNT(r.id) FILTER (WHERE r.status = 'completed')::bigint AS completed,
       COUNT(r.id) FILTER (WHERE r.status = 'failed')::bigint AS failed,
       COUNT(r.id) FILTER (WHERE r.status = 'cancelled')::bigint AS cancelled,
       COALESCE(SUM(r.input_tokens), 0)::text AS input_tokens,
       COALESCE(SUM(r.cached_input_tokens), 0)::text AS cached_input_tokens,
       COALESCE(SUM(r.output_tokens), 0)::text AS output_tokens,
       COALESCE(SUM(r.reasoning_output_tokens), 0)::text AS reasoning_output_tokens,
       COALESCE(SUM(r.total_tokens), 0)::text AS total_tokens,
       COALESCE(SUM(GREATEST(0, EXTRACT(EPOCH FROM LEAST(COALESCE(r.finished_at, sqlc.arg(as_of)::timestamptz), sqlc.arg(as_of)::timestamptz) - r.created_at))), 0)::double precision AS runtime_seconds
FROM runs r JOIN sessions s ON s.id = r.session_id
WHERE r.created_at >= sqlc.arg(period_from)::timestamptz AND r.created_at < sqlc.arg(period_to)::timestamptz
  AND (sqlc.narg(namespace)::text IS NULL OR s.namespace = sqlc.narg(namespace)::text)
GROUP BY s.namespace
ORDER BY runs_count DESC, s.namespace ASC NULLS LAST;
