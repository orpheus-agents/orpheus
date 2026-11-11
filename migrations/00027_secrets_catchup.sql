-- +goose Up
-- Validate both sources before moving either. No encryption key is needed.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM sessions
        WHERE env_ciphertext IS NOT NULL AND env_secret_id IS NULL
          AND (env_ciphertext !~ '^aes-gcm-v1:[A-Za-z0-9_-]{38,}$'
               OR (length(env_ciphertext) - 11) % 4 = 1)
    ) THEN
        RAISE EXCEPTION 'sessions.env_ciphertext has an unexpected format';
    END IF;
    IF EXISTS (
        SELECT 1 FROM runs
        WHERE env_ciphertext IS NOT NULL AND env_secret_id IS NULL
          AND (env_ciphertext !~ '^aes-gcm-run-v1:[A-Za-z0-9_-]{38,}$'
               OR (length(env_ciphertext) - 15) % 4 = 1)
    ) THEN
        RAISE EXCEPTION 'runs.env_ciphertext has an unexpected format';
    END IF;
END $$;
-- +goose StatementEnd

WITH picked AS (
    UPDATE sessions SET env_secret_id = gen_random_uuid()
    WHERE env_ciphertext IS NOT NULL AND env_secret_id IS NULL
    RETURNING env_secret_id, created_at, substr(env_ciphertext, 12) AS payload
), raw AS (
    SELECT env_secret_id, created_at,
           decode(rpad(translate(payload, '-_', '+/'),
                       (ceil(length(payload) / 4.0) * 4)::int, '='), 'base64') AS bytes
    FROM picked
)
INSERT INTO secrets(id, nonce, ciphertext, created_at)
SELECT env_secret_id, substring(bytes FROM 1 FOR 12), substring(bytes FROM 13), created_at
FROM raw;

WITH picked AS (
    UPDATE runs SET env_secret_id = gen_random_uuid()
    WHERE env_ciphertext IS NOT NULL AND env_secret_id IS NULL
    RETURNING env_secret_id, created_at, substr(env_ciphertext, 16) AS payload
), raw AS (
    SELECT env_secret_id, created_at,
           decode(rpad(translate(payload, '-_', '+/'),
                       (ceil(length(payload) / 4.0) * 4)::int, '='), 'base64') AS bytes
    FROM picked
)
INSERT INTO secrets(id, nonce, ciphertext, created_at)
SELECT env_secret_id, substring(bytes FROM 1 FOR 12), substring(bytes FROM 13), created_at
FROM raw;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM sessions WHERE env_ciphertext IS NOT NULL AND env_secret_id IS NULL)
       OR EXISTS (SELECT 1 FROM runs WHERE env_ciphertext IS NOT NULL AND env_secret_id IS NULL) THEN
        RAISE EXCEPTION 'env_ciphertext without secret after catchup';
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
-- Preserve table-only writes before earlier migrations can remove secrets.
UPDATE sessions s
SET env_ciphertext = 'aes-gcm-v1:' ||
    rtrim(translate(encode(x.nonce || x.ciphertext, 'base64'), E'+/\n', '-_'), '=')
FROM secrets x WHERE x.id = s.env_secret_id AND s.env_ciphertext IS NULL;

UPDATE runs r
SET env_ciphertext = 'aes-gcm-run-v1:' ||
    rtrim(translate(encode(x.nonce || x.ciphertext, 'base64'), E'+/\n', '-_'), '=')
FROM secrets x WHERE x.id = r.env_secret_id AND r.env_ciphertext IS NULL;
