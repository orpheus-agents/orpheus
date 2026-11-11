-- Generation only: Goose does not read this directory.
-- The following release removes these columns from the database.
ALTER TABLE sessions DROP COLUMN env_ciphertext;
ALTER TABLE runs DROP COLUMN env_ciphertext;
