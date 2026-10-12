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
