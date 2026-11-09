# Temporary service credentials (proposal)

A selected catalog service can obtain invocation-scoped credentials from a
server-side provider. The provider calculates data permissions; Orpheus never
interprets its scope. Connectors own the source, full result audience, input
identity, attachment handling, and delivery. Model settings and instructions stay
in ordinary profiles and workflows.

This proposal adds an optional mechanism. Existing `env_from` services and
requests without protected services keep their API and execution behavior.
It does not install a provider or migrate a running connector.

## Service configuration

```toml
[services.reports]
name = "Reports"
description = "Provider-authorized reports"

[services.reports.provider]
endpoint = "https://provider.example/credentials"
secret_env = "REPORTS_PROVIDER_CORE_KEY"
context_fields = ["subject", "scope_context", "context_observed_at", "source"]
token_file = "~/.reports/run.token"
# Optional per-service bounds, not universal provider limits:
max_lease_seconds = 300
max_run_seconds = 1200

[services.reports.provider.confirmation]
endpoint = "https://connector.example/source-confirmation"
secret_env = "REPORTS_CONNECTOR_CORE_KEY"
```

Select the code in `configuration.sandbox.services`. Run-level hook-only services
cannot issue credentials. Provider configuration is frozen with each accepted
grant. Its keys, and source confirmation keys, are excluded from all sandbox ENV
allowlists and cannot become the model's API key. HTTPS is required; redirects and
environment proxies are disabled. RPCs have a 60-second deadline and 64 KiB response
limit. Active worker RPCs are additionally bounded by the saved credential lease.
Endpoints, secret bindings and token paths come only from trusted configuration.

`context_fields` projects any subset of `subject`, `scope_context`,
`context_observed_at`, and `source`. There is no implicit projection. For the
existing Kord native `issue` endpoint select only `scope_context` and
`context_observed_at`: the current Provider rejects `subject`, `source` and
`confirmation`. That path retains Provider-owned native context validation; it
does not provide this proposal's source confirmation guarantee. The generic
confirmed path requires a separately reviewed Provider adapter. An isolated
opt-in candidate and a synthetic-source joint test now exist; this is not an
installed or agreed upstream interface. Do not strip
unknown fields and call that a confirmed integration.

The 300/1200-second limits above match the Kord pilot's current protocol. Omit a
bound (or use zero) to defer that maximum to another provider. Every grant must
still have a future lease ending no later than its absolute deadline. Five seconds
of clock tolerance apply only to configured upper bounds and source witness
future timestamps; never to expired grants. Source observation freshness remains
a Provider/source policy, not a universal five-second acceptance rule.

## Trusted context and ordinary sources

`CreateSession` and `CreateRun` optionally accept this context from authenticated
trusted callers:

```json
{
  "invocation_id": "a13d5917-7e7c-47a7-b4db-d8a792222038",
  "subject": {"namespace": "service", "id": "space-worker"},
  "scope_context": {"project": "support"},
  "context_observed_at": "2026-11-09T08:00:00Z",
  "source": {
    "identity": {"namespace": "space", "id": "schedule/17"},
    "revision": "occurrence/2026-11-09",
    "audience": []
  },
  "allowed_input_subjects": []
}
```

The initiating subject may be a human or service. The optional source identifies
an exact object/revision and its **complete** result audience. Readers may also be
services. An explicit empty audience is valid for a job with no result readers;
missing/null audience is unknown and rejected. A source needs a configured
confirmation authority even when it is not projected to the provider. Conversely,
a service requiring confirmation rejects admission without a source.

GitLab MR Review can identify a project/MR/head revision; Space can identify a
schedule occurrence. Neither needs a human initiator, a chat/channel/team/thread,
a separate role entity or transport routing. Existing Review and Space requests
without providers need no changes. Their protected integrations need source
adapters and release/client version pins; these are not implemented in this repo.

`scope_context` is a JSON object of at most 1 MiB, including `{}`. Orpheus preserves
large numbers and does not infer rights or input authority from its keys. Its
entire object is shared only with configured recipients: do not include prompts,
private messages, instructions, retrieved data, or secrets there.

The optional `allowed_input_subjects` list freezes permitted run steering
principals. Absent/empty denies every interactive input. The initiator need not
be listed. `SendMessage.access_context.input_subjects` supplies one trusted author
per message; the core checks namespaced identities, expiry and, when configured,
current source confirmation before accepting input. The list cannot be widened
through messages and is never sent to the credentials provider.

## Source confirmation

Before issue, before renewal, periodically while active (15 seconds between
successful checks), and on new input, the core asks the configured source authority:

```json
{
  "operation": "confirm",
  "context": {"...": "the exact original access_context"},
  "context_digest": "lowercase-sha256-hex"
}
```

Hash the UTF-8 bytes of the `context` JSON value **exactly as received**, without
reserializing. The core emits compact Go JSON with lossless raw scope values.
The authority must independently resolve the original source and compare its
revision, initiating identity and complete current audience with this snapshot.
Echoing a digest without checking the source is not confirmation. It returns:

```json
{"confirmed":true,"context_digest":"same-digest","observed_at":"2026-11-09T08:00:01Z"}
```

The witness must be at most 30 seconds old, no more than five seconds in the
future, and match the exact digest. Unknown fields/trailing JSON, partial or
changed audiences, a missing witness, failure and timeout all deny confirmation.
No caller/model can choose an endpoint in the access context.

The core passes a valid witness as `confirmation` on confirmed `issue` calls.
Providers see only configured context fields plus this witness, not the full
confirmation request or input allowlist. The witness authenticates the trusted
core's check; it is not a standalone signature for other callers.

Providers needing confirmation on every protected operation call the
server-authenticated `POST /api/v1/service-access/confirm` with only
`{"invocation_id":"..."}`. It uses saved authority, rejects cancelled, terminal,
revoked, or expired invocations, rechecks the source, then rechecks run liveness.
An audience denial requests cancellation. This callback does not accept new
readers, scopes or identities. Periodic checks alone do not guarantee a fresh
roster at the instant of every data operation or later connector publication.

## Provider lifecycle

| Operation | Request | Response / invariants |
| --- | --- | --- |
| `issue` | Fresh request ID, service, invocation ID, configured fields, optional confirmed witness | `credential_id`, `access_token`, `scope_key`, `expires_at`, `run_expires_at` |
| `renew` | Stable request ID derived from invocation/service/credential/previous lease, and those identifiers | Same bearer, credential ID, scope and absolute deadline; lease cannot decrease |
| `revoke` | Stable request ID derived from invocation/service/credential, and those identifiers | Exactly `{"revoked":true}`; retries confirm the same revoked grant |

These are opt-in protocol requirements, not rules for every external credential
system. Stable bearers are necessary for this version's file installation and
restart behavior; rotating-token providers require an explicit adapter/protocol
extension. Stable scope and absolute deadline prevent old model context from
acquiring broader access or a fresh lifetime. Revoke must be permanent and win
against concurrent renew/replay. Providers own rights reduction, identity mapping,
audience-to-data intersection, and prevention of widening an existing scope.

Provider 401/403 maps to `403 service_access_denied`, conflict to
`409 service_credentials_conflict`, other failures/malformed data to
`503 service_credentials_unavailable`. Error bodies and tokens never become
public errors. Source failures use `403 audience_unconfirmed`. The core does not
invent a generic ordering for opaque `scope_key` values.

## Persistence, execution and continuation

Migration 25 adds credential/input audit tables after current upstream migration
24; it contains no pilot migration-number shims. Grants and frozen configuration
use AES-GCM with separate session/run-bound encryption. Public views/events do not
contain tokens. Admission RPCs run outside database locks; accepted idempotent
replays work during Provider outage. Invocation reuse with another accepted run
is rejected. Fingerprints canonicalize scope JSON without losing number precision.

Before hooks/model start the worker stages `0600 run.token` and `0400 run.json`
in a `0700` directory and publishes metadata last. Metadata contains only schema
1, core run ID, invocation ID and absolute deadline. It is an aid for tools, not
an authorization authority: code running as the sandbox user can edit its files.
Cancellation removes both files and stops renewal. Terminal/cancelled accepted
grants stay in a durable retry queue until Provider revocation is confirmed,
independently of sandbox pause/deletion. Failed batches move behind other cleanup
attempts; a worker restart retries the same revoke ID. Provider outage never
blocks local sandbox cleanup waiting for remote revocation.

The first model dispatch persists the earlier of workflow execution timeout and
Provider absolute deadline minus the configured cancellation/polling reserve.
Preparation consumes the original lifetime. Renew/restart do not move this saved
deadline. Restoration loads the saved grant, never reissues into old memory.
Expired or unconfirmed runs stop and cannot resume their old sandbox state.

Continuation requires the same complete protected-service set, provider
configuration (projection order irrelevant), opaque scope keys, source identity
and complete result audience. A new source/audience needs clean memory even if a
Provider returns the same scope key. Source revision alone may advance. Changes
return `409 service_scope_changed`; the connector creates a clean session and
owns history/attachment boundaries. Ordinary runs have no source requirement.

Admission that has not committed has no accepted-run cleanup record: unknown
issue outcomes and grants rejected by concurrent admission remain bounded by
Provider expiry. Durable pre-admission issuance reconciliation is a separate
extension; this proposal does not claim immediate cleanup of unknown grants.

## Verification and release boundary

Run `make generate`, `make fix gofix check`. The optional cross-repository test
`TestPinnedProviderNativeWireCompatibility` takes `ORPHEUS_PROVIDER_TEST_URL` and
uses the real Provider/SQLite, Core HTTP handler, PostgreSQL and deterministic
sandbox/model simulator. It checks native projection, bearer installation,
renewal, cancellation, revocation, revoke replay, and denial of unsupported fields.
It does not establish generic source support in the current Kord Provider.

The additional opt-in cross-repository `TestGenericProviderA9` uses an actual
versioned Provider candidate/SQLite, Core HTTP/store/worker code and PostgreSQL.
A synthetic authoritative registry independently checks original source identity,
revision, initiator, scope, complete audience and frozen input authority. It
checks the Provider projection separately and binds it to the original full Core
context digest; neither endpoint approves an echo alone. Real HTTPS callbacks
use explicitly trusted fixture CAs. Sandbox/model execution, identity/policy facts
and source records remain synthetic.

Its 15 cases cover human and GitLab/Space service principals, issue/renew, a
Provider-to-Core callback before sensitive access, worker restoration, graceful
cancellation and durable revoke/replay, source revision/audience changes, forged
author/input/source snapshots, rights reduction, compatible revision continuation,
new source/audience memory boundaries and expiry. A synthetic service principal
has no employee identity and uses an explicit existing policy; no human or MM
object is fabricated. The current Kord candidate denies empty audiences without
a separate service-only policy, which is tested as a distinct refusal.

Provider expiry is exercised by an explicit fixture clock advance. Core outage
expiry uses a saved lease moved into the past, rather than waiting five minutes.
A fresh Provider instance reopens the ledger for each request; a new Core executor
restores its authority from PostgreSQL. These are recovery checks, not an OS crash
or live delivery test. Source correctness is established only for this synthetic
registry; the candidate interface is not an upstream agreement.

Live AgentBox, real source authority correctness, actual business operations,
connector delivery, source changes during publication and rollout remain separate
acceptance checks. This proposal changes no running bot, service, permission,
secret, queue, or published upstream PR.
