# Secret storage

Explicit session and run environment maps are stored as AES-256-GCM payloads
in `secrets`, referenced by `sessions.env_secret_id` and `runs.env_secret_id`.
An empty map creates neither a secret nor a reference. Values are immutable.
The encryption key is `ENV_ENCRYPTION_KEY`, required by both API and worker.
Service values selected through `env_from` come from worker ENV.

Session payloads authenticate the session UUID. Run payloads authenticate
`orpheus-run-env-v1\0`, the session UUID, and the run UUID. Moving a reference to
another owner's secret causes decryption to fail.

## Writes and reads

Session and run creation write the encrypted value and its reference in the
owner's transaction. Failed admission rolls back the secret as well. Readers
use only the referenced secret; a NULL reference means no explicit environment.
Missing or damaged referenced secrets fail closed. API replays report
`503 storage_unavailable`. Temporary database read errors are retried by the
worker and do not consume hook launch attempts. Missing or invalid encrypted
data fails the run or hook.

## Integrity checks

Each secret must be referenced by a session or run. This read-only query must
return zero:

```sql
SELECT count(*) AS orphan_secrets FROM secrets x
WHERE NOT EXISTS (SELECT 1 FROM sessions s WHERE s.env_secret_id = x.id)
  AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.env_secret_id = x.id);
```

This checks references, not ciphertext authentication or the encryption key.

## Upgrading an existing installation

For existing installations, follow the
[v0.9.0 upgrade instructions](https://github.com/orpheus-agents/orpheus/blob/v0.9.0/docs/secret-storage.md),
including their prerequisites and reconciliation checks. Every API and worker
must reach v0.9.0 before applying migration 28 (the cleanup migration).
It refuses missing references or mismatching legacy copies before deleting
either column. On fresh installations, `migrate up` applies the full chain.

The cleanup migration limits each lock wait to five seconds. Waiting for both
tables can take about ten seconds and temporarily block API and worker queries. If a long
transaction prevents the migration, both columns remain intact; resolve the
blocking transaction and retry before continuing the rollout.

## Rollback

Rolling back the image to v0.9.0 requires no schema rollback. For v0.8.0,
stop writers and run `migrate down` once using the image that applied migration
28 before starting the older version. This restores both legacy columns and their values without
the encryption key. Earlier versions require a separate offline rollback plan.
