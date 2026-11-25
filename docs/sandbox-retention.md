# Paused sandbox retention

Paused reusable sandboxes are deleted after `SANDBOX_PAUSED_RETENTION_DAYS`
(default 14) days since the last run finished, including failed and cancelled runs.
The worker checks on startup and every four hours, selects at most
`SANDBOX_CLEANUP_BATCH_SIZE` (default 1000) sessions per pass including retries,
and deletes up to three concurrently. Both settings require positive integers; retention days must be between 1 and 100.
A backlog can delay deletion beyond the retention period. Active or reserved
sessions are excluded; cleanup does not reserve execution capacity.

Deletion never resumes a sandbox. Failed or uncertain deletions are retried in
later passes, including after restart. Session history, results and usage remain
available. The sandbox moves through `deleting` to `deleted`, with
`sandbox_expired` in `sandbox.error`; deletion failures use `sandbox_delete_failed`.
New runs in deleting, deleted or unavailable sessions receive HTTP 409
`session_unavailable`. Create a new session with the required context. If an
external deletion is discovered after acceptance, the run fails asynchronously
with `sandbox_lost`. Replaying an accepted idempotency key still returns the
original acceptance. Deploy connectors that handle `deleting` and `deleted`
reusable sessions before deploying this worker version.

## Resuming a sandbox for a new run

The worker gives the first connection to an existing sandbox the same time
budget as initial creation. `SANDBOX_PROVISION_TIMEOUT_SECONDS` defaults to 120.
The deadline applies to both the state check and the connection. Transient
errors are retried with backoff. The journal preserves the deadline and retry
schedule across worker restarts, including upgrades from an earlier release.

If the sandbox cannot be reached in time, the run fails during preparation
with `sandbox_resume_timeout`. Cancellation finishes locally even during a
provider outage. The session keeps its sandbox ID and workspace. A late
connection cannot start a hook or dispatch the assignment. The worker settles
the sandbox according to the session lifecycle. Confirmed loss of the sandbox
produces `sandbox_lost`.

The first provider failure records the method, error type and available HTTP
status. The terminal resume log includes the operation ID, attempt count and
elapsed time. After the first successful connection, hook execution and agent
recovery follow their own deadlines.
