-- +goose Up
ALTER TABLE sessions ADD COLUMN namespace text, ADD COLUMN external_key text;
ALTER TABLE runs ADD COLUMN input_fingerprint text;
ALTER TABLE messages ADD COLUMN external_key text;
CREATE INDEX sessions_external ON sessions(namespace, external_key, created_at, id);
CREATE INDEX sessions_external_key ON sessions(external_key, created_at, id);
CREATE INDEX sessions_created ON sessions(created_at, id);
CREATE INDEX runs_fingerprint ON runs(input_fingerprint, created_at, id);
CREATE INDEX runs_created ON runs(created_at, id);
CREATE INDEX runs_status_created ON runs(status, created_at, id);
CREATE INDEX messages_external ON messages(session_id, external_key) WHERE role = 'user';

-- +goose Down
DROP INDEX messages_external;
DROP INDEX runs_status_created;
DROP INDEX runs_created;
DROP INDEX runs_fingerprint;
DROP INDEX sessions_created;
DROP INDEX sessions_external_key;
DROP INDEX sessions_external;
ALTER TABLE messages DROP COLUMN external_key;
ALTER TABLE runs DROP COLUMN input_fingerprint;
ALTER TABLE sessions DROP COLUMN external_key, DROP COLUMN namespace;
