-- name: RunsByNativeIDs :many
SELECT * FROM runs WHERE session_id = $1 AND native_turn_id = ANY(sqlc.arg(turn_ids)::text[]) ORDER BY number;
