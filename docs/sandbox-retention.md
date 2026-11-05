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
