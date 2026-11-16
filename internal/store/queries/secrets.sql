-- name: CreateSecret :exec
INSERT INTO secrets(id, nonce, ciphertext) VALUES ($1, $2, $3);

-- name: GetSecret :one
SELECT nonce, ciphertext FROM secrets WHERE id = $1;

-- name: UpdateSecret :execrows
UPDATE secrets SET nonce=$2, ciphertext=$3 WHERE id=$1;

-- name: DeleteSecret :execrows
DELETE FROM secrets WHERE id=$1;
