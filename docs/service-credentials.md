# Temporary service credentials

A provider confirms its own data scope and issues a short-lived bearer for one
invocation. Orpheus manages admission, encryption, renewal and the sandbox token
file. Data-specific access rules belong to the provider. Connectors collect trusted
context and choose who may send further input. This mechanism does not enable a
workflow or connect a production channel by itself.

## Configuration and provider projection

```toml
[services.records]
name = "Records"
description = "Records available to the invocation scope"

[services.records.provider]
endpoint = "https://provider.example/api/v1/agent-credentials"
secret_env = "RECORDS_PROVIDER_KEY"
context_fields = ["subject", "scope_context", "context_observed_at"]
token_file = "~/.records/run.token"
```

`context_fields` selects any subset of the three supported fields, including an
empty subset. Duplicate or unsupported names are rejected. Every issue request
always contains `operation`, a fresh `request_id`, `service` and `invocation_id`;
it adds only the configured context fields. `allowed_input_subjects`, prompts,
message metadata, model output and retrieved data are never projected from other
request fields.

`scope_context` is shared minimal trusted context, not a container for prompts,
retrieved data or secrets intended for one provider. Selecting it forwards the
whole object; providers sharing a run must be permitted to receive that object.
Orpheus does not inspect provider-specific keys or derive permissions from them.

The server secret must contain at least 32 bytes and is available only to the API
and worker. It is excluded from sandbox environment selection, even if it appears
in an environment allowlist. Providers require HTTPS; redirects and environment
proxies are disabled. Responses are limited to 64 KiB and requests to ten seconds.
Provider error bodies are not returned to clients. Configuration fixes the token
path as `~/.<service>/run.token`; provider responses cannot choose paths, commands
or environment variables.

Select protected services in `configuration.sandbox.services` so hooks and the
model can use the token file. Run-level `services` are available only to hooks and
reject providers. Existing `env_from` services keep their behavior. Runs with a
provider cannot exceed 1200 seconds.

## Trusted invocation context

`CreateSession` and `CreateRun` accept `access_context` from callers authenticated
with the server API key:

```json
{
  "invocation_id": "a13d5917-7e7c-47a7-b4db-d8a792222038",
  "subject": {"namespace": "scheduler", "id": "nightly-review"},
  "scope_context": {"repository": "support", "merge_request_iid": 42},
  "context_observed_at": "2026-10-12T09:00:00Z",
  "allowed_input_subjects": [{"namespace": "gitlab", "id": "reviewer"}]
}
```

`subject` identifies the initiating human or service principal. Its namespace and
ID are nonempty, bounded identifiers. `scope_context` is an opaque JSON object,
including an empty object, limited to 1 MiB when serialized. The API preserves
large JSON numbers. No channel, bot, audience or policy-space fields are required:
cron jobs, merge requests, tickets and messaging connectors use the same core
contract. Providers validate their own context and its observation time.

`allowed_input_subjects` is an optional, frozen list of up to 4096 unique
`{namespace,id}` principals permitted to use `SendMessage` during this run. Both
parts must match. An absent or empty list denies every interactive input. The
initiator need not be in this list: a scheduled service can start a run that only
named reviewers may steer. Provider scope, message text and metadata never grant
input authority. Old saved contexts without this explicit list also deny inputs;
there is no fallback to an audience field inside `scope_context`.

## Admission and continuation

Admission first checks for an accepted replay, issues credentials outside the
transaction, then rechecks idempotency, capacity and scope under the normal locks
before atomic persistence. Accepted requests replay while a provider is offline.
One invocation can be accepted only once, even with a different operation key.
Busy or capacity responses do not trigger automatic renewal.

The idempotency fingerprint includes invocation context and input authority.
Nested scope object ordering and equivalent decimal forms are canonicalized
without losing number precision or modifying the object sent to providers.

Continuation compares the complete protected service set, each `scope_key` and
its configured endpoint, server secret binding, token path and context projection.
Projection order is irrelevant; changing its fields requires a new session even
when the provider returns the same scope key. A legacy session without a protected
scope cannot acquire protected credentials under its existing memory. A different
initiator with the same confirmed scope may continue the session.

An incompatible scope returns `409 service_scope_changed`. The connector creates
a clean regular session with the same invocation and excludes prior history and
files. History boundaries and attachment checks remain the connector's concern.

`SendMessage.access_context.input_subjects` supplies the confirmed author of each
message in order. Orpheus checks every author against the run's saved explicit
allowlist, without another issue request or a fresh permission calculation. The
input audit and messages are persisted atomically. Connectors must check authors
before preparing attachments in the sandbox.

## Lifetime and storage

Migration 23 adds credentials and input-author auditing. Grants and server
configuration are encrypted using the existing AES-GCM `ENV_ENCRYPTION_KEY`, a
separate encryption domain and session/run bindings. Ordinary database columns
hold only invocation context and scopes. Tokens do not enter public APIs, events,
environment variables or shell commands.

Before hooks run, the worker atomically installs a `0600` file in a `0700`
directory. Renewal starts 60 seconds before expiry and preserves the bearer,
credential ID, scope and absolute deadline. Its operation key derives from the
saved lease, so a lost response can be retried. The lease is at most 300 seconds;
the absolute run lifetime is at most 1200 seconds.

The response check tolerates a provider clock up to five seconds ahead, solely
when bounding future timestamps. It preserves all grant timestamps, rejects
expired credentials and never extends the execution deadline. Providers enforce
their own leases. Provider failures leave the existing expiry unchanged; there is
no permanent-key fallback.

Restart restores the saved grant while valid; it does not issue again under old
model memory. An expired lease stops the sandbox, fails the run with
`service_credentials_expired` and closes the session to further runs. Unconfirmed
model state after that stop cannot continue automatically. Cancellation stops
renewal. Normal cleanup removes the file before pausing, or deletes a single-run
sandbox. There is no required close/publication callback. Responses and files use
the invocation's initial rights snapshot; later rights apply to the next run.

## Verification

Run `make fix gofix check`. This includes API and SQL generation checks, unit and
PostgreSQL integration tests, migration up/down tests, race detection, linters,
vulnerability scans and compilation. Credential tests cover generic cron/MR/ticket
contexts, lossless JSON and canonical replay, frozen namespaced input authority,
empty-list denial, provider projection, encryption, concurrent admission,
accepted replay during provider outage, author/scope/projection changes,
invocation reuse, worker recovery, renewal, expiry and token-file write failures.

Live AgentBox, a real provider and end-to-end connector delivery need separate
acceptance.
