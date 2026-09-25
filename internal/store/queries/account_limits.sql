-- name: UpsertAccountLimitObservation :exec
INSERT INTO account_limit_observations (
    account_id, source_fingerprint, last_attempt_at, last_success_at, last_error_code, snapshot
) VALUES (
    @account_id, @source_fingerprint, @observed_at,
    CASE WHEN sqlc.narg(error_code)::text IS NULL THEN @observed_at::timestamptz ELSE NULL END,
    sqlc.narg(error_code)::text, sqlc.narg(snapshot)::jsonb
)
ON CONFLICT (account_id, source_fingerprint) DO UPDATE SET
    last_attempt_at = EXCLUDED.last_attempt_at,
    last_success_at = COALESCE(EXCLUDED.last_success_at, account_limit_observations.last_success_at),
    last_error_code = EXCLUDED.last_error_code,
    snapshot = COALESCE(EXCLUDED.snapshot, account_limit_observations.snapshot)
WHERE account_limit_observations.last_attempt_at < EXCLUDED.last_attempt_at;

-- name: ListAccountLimitObservations :many
SELECT account_id, source_fingerprint, last_attempt_at, last_success_at, last_error_code, snapshot
FROM account_limit_observations
WHERE account_id = ANY(@account_ids::text[]);
