-- name: CreateSecret :exec
INSERT INTO secrets(id, nonce, ciphertext) VALUES ($1, $2, $3);

-- name: GetSecret :one
SELECT nonce, ciphertext FROM secrets WHERE id = $1;
