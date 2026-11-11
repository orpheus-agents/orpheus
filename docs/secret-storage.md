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

Migration 28 removes the legacy `sessions.env_ciphertext` and
`runs.env_ciphertext` columns. sqlc and Goose both use `migrations` as their
schema source, including on fresh installations. The generation check rejects
any `env_ciphertext` reference in generated SQL code.

Migrations 25 and 26 introduce the schema and copy existing ciphertext.
Migration 27 catches up legacy-only rows written during the dual-write rollout
and verifies that none remain. Migration 28 checks that every non-NULL legacy
copy has a reference and exactly matches its secret before dropping either
column. Any failed check rolls back the entire migration. Neither direction
needs the encryption key or changes the encrypted bytes.

## Upgrade prerequisites

Upgrade to this release only after every API and worker runs v0.9.0 or later.
Versions through v0.8.0 still reference the legacy columns and cannot run after
migration 28. Complete the v0.9.0 rollout and reconciliation first.

Migration 28 sets a five-second timeout for each lock wait, not for the entire
migration. The two column drops can wait up to about ten seconds in total;
while waiting on runs, the migration retains its exclusive lock on sessions.
A long transaction can prevent the column removal; in that case the migration
fails with both columns intact.
Resolve the blocking transaction and retry the migration before continuing
the rollout.

## Checks before migration 28

Run these read-only queries after the v0.9.0 rollout, before migration 28.
Every counter must be zero. The mismatch query checks only non-NULL legacy
copies: NULL is expected for table-only writes. The first two queries cannot
run after migration 28 because the columns no longer exist.

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

## Checks after migration 28

Verify that the migration reached version 28, all API and worker processes are
healthy, and both counters below are zero:

```sql
SELECT count(*) AS legacy_columns FROM information_schema.columns
WHERE table_schema = current_schema()
  AND table_name IN ('sessions', 'runs') AND column_name = 'env_ciphertext';

SELECT count(*) AS orphan_secrets FROM secrets x
WHERE NOT EXISTS (SELECT 1 FROM sessions s WHERE s.env_secret_id = x.id)
  AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.env_secret_id = x.id);
```

## Rollback

An image rollback to v0.9.0 needs no schema rollback: that release already uses
table-only queries. Rolling back to v0.8.0 requires reverting migration 28 first.
Stop application writers and run `migrate down` once using this release's image,
then start v0.8.0. The older image does not contain migration 28 and cannot
revert it.

Migration 28's Down recreates both legacy columns and fills them from `secrets`,
including table-only writes. Each value has the original prefix and unpadded
base64url encoding of nonce followed by ciphertext. Empty environments remain
NULL. References, secret IDs, timestamps and encrypted bytes stay unchanged.

An image rollback to a release before v0.8.0 is unsupported without an offline
schema rollback. To reach schema version 24, stop writers and run
`migrate down` four times with this release's image (28, 27, 26, then 25).
Down is not the normal image rollback procedure.
