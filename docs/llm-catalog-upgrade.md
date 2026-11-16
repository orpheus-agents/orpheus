# Upgrade to database LLM catalogs

Migration 30 creates the catalogs and session account reference. Migration 31 is
a one-time transactional data import executed by the normal `orpheus migrate up`.
There is no separate import command and no runtime support for TOML profiles.

## New installations

Run normal migrations without `ORPHEUS_MIGRATION_CONFIG_FILE`. If there are no
old sessions or limit observations, the import succeeds with empty catalogs.
Start API and worker with infrastructure TOML, then create accounts, profiles and
templates through the API or UI. No legacy profile configuration is needed.

## Existing installations

1. Back up the database. Preserve the complete old TOML in a separate file.
   Every profile must have a model. Keep configured credential stores unchanged.
   Prepare the runtime TOML by removing `[profiles.*]`; retain credential stores
   and services.
2. Stop admission, wait for active runs to finish, then stop **both** old API and
   worker. Ensure no old replicas can restart. Do not run mixed versions against
   the database during or after import.
3. Run the new image's normal `migrate up` with `DATABASE_URL`,
   `ORPHEUS_MIGRATION_CONFIG_FILE=/path/to/old.toml`, `ENV_ENCRYPTION_KEY` and every
   API-key environment variable referenced by current profiles or still-needed
   old sessions. Mount the old file only in the migration process. The import
   does not access S3, download auth files or move objects.
4. Check migration success, then start new API and worker with the reduced runtime
   TOML. Check the accounts, profiles, observations and session continuation.
5. Remove the temporary import file, mount, environment variables and rollout
   coordination from subsequent deployments. Later `migrate up` runs skip the
   applied import without opening the old file or looking for legacy keys.

The migration imports current profiles and deduplicates their resolved credential
sources into active accounts. It retains old S3 paths. Sources needed by sessions
but absent from current TOML become ordinary inactive accounts. Profile and
account renames do not determine source matching; current profile rebinding is
never applied to old sessions. Execution parameters remain unchanged while
`configuration.credentials` is removed.

A session may have a null account only when its sandbox is deleted, its slot is
released, all runs are terminal, no hook/operation/provisioning work remains, and
its source does not match an imported account. All other sessions must have a
resolvable source. A missing store, missing required API-key environment value,
ambiguous source or observation collision aborts the data transaction. Correct
the reported input and rerun normal migrations; the empty schema from migration
30 can remain in place. Never fabricate credentials to pass the check.

The data import cannot be reversed with `migrate down`. Rollback requires
restoring the pre-upgrade database together with the old application and TOML.
The schema migration can be reversed before the data import has been applied.
