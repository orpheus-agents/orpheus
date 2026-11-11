# Secret storage

Explicit session and run environment maps are stored as AES-256-GCM payloads
in `secrets`, referenced by `sessions.env_secret_id` and `runs.env_secret_id`.
An empty map creates neither a secret nor a reference. Values are immutable.
The encryption key remains `ENV_ENCRYPTION_KEY`, required by both API and worker.
Service values selected through `env_from` still come from worker ENV.

Session payloads authenticate the session UUID. Run payloads authenticate
`orpheus-run-env-v1\0`, the session UUID, and the run UUID. Moving a reference to
another owner's secret causes decryption to fail.

## Transitional writes and reads

This release writes both the secret and the legacy `env_ciphertext` column in
the owner's transaction. Both contain the same encrypted bytes: encryption runs
once. Failed admission rolls back the secret as well.

Readers use the referenced secret first. They read the legacy column only when
the reference is absent, to support records created by an older process during
rollout. A missing or damaged referenced secret is an error; it never causes a
fallback to the column. API replays report `503 storage_unavailable`.
Temporary database read errors are retried by the worker and do not consume hook
launch attempts. Missing or invalid encrypted data fails the run or hook.

Migrations 25 and 26 introduce the schema and copy existing ciphertext. No key
is needed: the copy separates the nonce from the authenticated ciphertext without
decrypting or re-encrypting it. Malformed source data aborts the data migration.
The copy is repeatable and preserves the owner's creation time. Schema and data
changes are separate transactions.

## Rollout checks

After replacing all API and worker processes, run these read-only queries.
The number of legacy-only records must stop growing. It can remain nonzero:
older processes may have written between the migration and their replacement.
The next migration stage will copy these remaining records.

```sql
SELECT 'sessions' AS owner, count(*) AS legacy_only
FROM sessions WHERE env_ciphertext IS NOT NULL AND env_secret_id IS NULL
UNION ALL
SELECT 'runs', count(*)
FROM runs WHERE env_ciphertext IS NOT NULL AND env_secret_id IS NULL;

SELECT 'sessions' AS owner, count(*) AS mismatches
FROM sessions s JOIN secrets x ON x.id = s.env_secret_id
WHERE s.env_ciphertext IS DISTINCT FROM 'aes-gcm-v1:' ||
    rtrim(translate(encode(x.nonce || x.ciphertext, 'base64'), E'+/\n', '-_'), '=')
UNION ALL
SELECT 'runs', count(*)
FROM runs r JOIN secrets x ON x.id = r.env_secret_id
WHERE r.env_ciphertext IS DISTINCT FROM 'aes-gcm-run-v1:' ||
    rtrim(translate(encode(x.nonce || x.ciphertext, 'base64'), E'+/\n', '-_'), '=');

SELECT count(*) AS orphan_secrets FROM secrets x
WHERE NOT EXISTS (SELECT 1 FROM sessions s WHERE s.env_secret_id = x.id)
  AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.env_secret_id = x.id);
```

Mismatches and orphan secrets must be zero. These checks compare ciphertext;
they do not authenticate it or validate the master key.

## Rollback

The preceding application version can read every record written by this release
from `env_ciphertext`. An image rollback does not require reverting the schema.
If reverting the schema as well, first stop all processes that use `env_secret_id`.
The data migration's Down keeps both copies; the schema migration's Down drops
the references and table, leaving the legacy ciphertext intact.

Deploy the table-only reader/writer and removal of the legacy columns as separate
later releases. Do not skip the intervening application rollout.
