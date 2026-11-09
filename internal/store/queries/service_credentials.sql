-- name: GetServiceCredentials :one
SELECT * FROM run_service_credentials WHERE run_id = $1;

-- name: InsertServiceCredentials :exec
INSERT INTO run_service_credentials (run_id, access_context, scopes, ciphertext, invocation_id)
VALUES ($1, $2, $3, $4, $5);

-- name: UpdateServiceCredentials :exec
UPDATE run_service_credentials SET ciphertext = $2 WHERE run_id = $1;

-- name: InsertInputSubject :exec
INSERT INTO run_input_subjects (message_id, subject) VALUES ($1, $2);

-- name: InvocationAccepted :one
SELECT EXISTS(SELECT 1 FROM run_service_credentials WHERE invocation_id = $1);

-- name: PendingCredentialRevocations :many
SELECT r.session_id, c.run_id FROM run_service_credentials c
JOIN runs r ON r.id=c.run_id
WHERE c.revoked_at IS NULL
AND (r.status IN ('completed','failed','cancelled') OR r.cancel_requested_at IS NOT NULL)
ORDER BY c.revoke_attempted_at NULLS FIRST, r.created_at, c.run_id LIMIT 100;

-- name: MarkCredentialsRevoked :exec
UPDATE run_service_credentials SET revoked_at=now() WHERE run_id=$1;

-- name: InvocationRun :one
SELECT r.session_id, r.id, r.status, r.cancel_requested_at
FROM runs r JOIN run_service_credentials c ON c.run_id=r.id
WHERE c.invocation_id=$1 AND c.revoked_at IS NULL;

-- name: MarkRevocationAttempt :exec
UPDATE run_service_credentials SET revoke_attempted_at=now() WHERE run_id=$1;
