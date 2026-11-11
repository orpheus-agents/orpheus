-- +goose Up
SET LOCAL lock_timeout = '5s';

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM sessions WHERE env_ciphertext IS NOT NULL AND env_secret_id IS NULL)
     OR EXISTS (SELECT 1 FROM runs WHERE env_ciphertext IS NOT NULL AND env_secret_id IS NULL) THEN
    RAISE EXCEPTION 'env_ciphertext without secret: run the RFC-033 migration first';
  END IF;

  IF EXISTS (
    SELECT 1 FROM sessions s JOIN secrets x ON x.id = s.env_secret_id
    WHERE s.env_ciphertext IS NOT NULL
      AND s.env_ciphertext <> 'aes-gcm-v1:' ||
          rtrim(translate(encode(x.nonce || x.ciphertext, 'base64'), E'+/\n', '-_'), '=')
  ) OR EXISTS (
    SELECT 1 FROM runs r JOIN secrets x ON x.id = r.env_secret_id
    WHERE r.env_ciphertext IS NOT NULL
      AND r.env_ciphertext <> 'aes-gcm-run-v1:' ||
          rtrim(translate(encode(x.nonce || x.ciphertext, 'base64'), E'+/\n', '-_'), '=')
  ) THEN
    RAISE EXCEPTION 'env_ciphertext differs from its secret';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE sessions DROP COLUMN env_ciphertext;
ALTER TABLE runs     DROP COLUMN env_ciphertext;

-- +goose Down
ALTER TABLE sessions ADD COLUMN env_ciphertext text;
ALTER TABLE runs     ADD COLUMN env_ciphertext text;

UPDATE sessions s
SET env_ciphertext = 'aes-gcm-v1:' ||
    rtrim(translate(encode(x.nonce || x.ciphertext, 'base64'), E'+/\n', '-_'), '=')
FROM secrets x WHERE x.id = s.env_secret_id;

UPDATE runs r
SET env_ciphertext = 'aes-gcm-run-v1:' ||
    rtrim(translate(encode(x.nonce || x.ciphertext, 'base64'), E'+/\n', '-_'), '=')
FROM secrets x WHERE x.id = r.env_secret_id;
