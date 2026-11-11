# Secret storage

Explicit session and run environment maps are stored as AES-256-GCM payloads
in `secrets`, referenced by `sessions.env_secret_id` and `runs.env_secret_id`.
An empty map creates neither a secret nor a reference. Values are immutable.
The encryption key remains `ENV_ENCRYPTION_KEY`, required by both API and worker.
Service values selected through `env_from` still come from worker ENV.

Session payloads authenticate the session UUID. Run payloads authenticate
`orpheus-run-env-v1\0`, the session UUID, and the run UUID. Moving a reference to
another owner's secret causes decryption to fail.

## Table-only writes and reads

Session and run creation write the encrypted value and its reference in the
owner's transaction. Failed admission rolls back the secret as well. Readers
use only the referenced secret; a NULL reference means no explicit environment.
Missing or damaged referenced secrets fail closed. API replays report
`503 storage_unavailable`. Temporary database read errors are retried by the
worker and do not consume hook launch attempts. Missing or invalid encrypted
data fails the run or hook.

The legacy `env_ciphertext` columns remain in PostgreSQL for the preceding
release during rollout. New records leave them NULL. They are absent from the
application models and generated queries: sqlc applies the generation-only
`internal/store/schema_pending` directory after `migrations`. Goose reads only
`migrations`, including on a fresh installation. The generation check rejects
any `env_ciphertext` reference in generated SQL code.

Migrations 25 and 26 introduce the schema and copy existing ciphertext.
Migration 27 catches up legacy-only rows written during the preceding rollout
and verifies that none remain. No key is needed: the copy separates the nonce
from the authenticated ciphertext without decrypting or re-encrypting it.
Malformed source data or remaining legacy-only rows abort migration 27 and
roll back its changes. Existing secret references and values stay intact.

Upgrade only after every API and worker has reached the preceding dual-write
release and its rollout checks are clean. Migration 27 runs before replacing
those processes; they always write the secret too, so no new legacy-only rows
can appear during this rollout.

## Rollout checks

After replacing all API and worker processes, run these read-only queries.
Every counter must be zero. The mismatch query checks only non-NULL legacy
copies: NULL is expected for records created by this release.

```sql
SELECT 'sessions' AS owner, count(*) AS legacy_only
FROM sessions WHERE env_ciphertext IS NOT NULL AND env_secret_id IS NULL
UNION ALL
SELECT 'runs', count(*)
FROM runs WHERE env_ciphertext IS NOT NULL AND env_secret_id IS NULL;

SELECT 'sessions' AS owner, count(*) AS mismatches
FROM sessions s JOIN secrets x ON x.id = s.env_secret_id
WHERE s.env_ciphertext IS NOT NULL
  AND s.env_ciphertext IS DISTINCT FROM 'aes-gcm-v1:' ||
    rtrim(translate(encode(x.nonce || x.ciphertext, 'base64'), E'+/\n', '-_'), '=')
UNION ALL
SELECT 'runs', count(*)
FROM runs r JOIN secrets x ON x.id = r.env_secret_id
WHERE r.env_ciphertext IS NOT NULL
  AND r.env_ciphertext IS DISTINCT FROM 'aes-gcm-run-v1:' ||
    rtrim(translate(encode(x.nonce || x.ciphertext, 'base64'), E'+/\n', '-_'), '=');

SELECT count(*) AS orphan_secrets FROM secrets x
WHERE NOT EXISTS (SELECT 1 FROM sessions s WHERE s.env_secret_id = x.id)
  AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.env_secret_id = x.id);
```

Mismatches and orphan secrets must be zero. These checks compare ciphertext;
they do not authenticate it or validate the master key.

## Rollback

An image rollback to the preceding dual-write release (v0.8.0) is safe without
reverting migrations: that release reads the referenced secret first, including
for records with a NULL legacy column. An image rollback to a release before
v0.8.0 is unsupported because those readers ignore the secret table.

Migration 27's Down restores missing legacy values from the secret table,
preserving existing copies. This makes an offline schema rollback through
migrations 26 and 25 lossless for table-only writes too. Stop application writers
before schema rollback; Down is not the normal image rollback procedure.
Use this release's image for the rollback commands: v0.8.0 does not contain
migration 27 and cannot roll it back. To reach schema version 24, run
`migrate down` three times with this release's image (27, 26, then 25), and
only then start the older application image.

Remove the legacy columns in a separate later release, after every process
uses table-only queries. The later release also removes `schema_pending` because
the real migration schema will then omit the columns.
