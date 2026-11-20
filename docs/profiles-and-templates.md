# Accounts, profiles and templates

LLM accounts, model profiles and sandbox templates are PostgreSQL catalogs.
Create them through Orpheus Web or the API; changes take effect without restarting
API or worker. Empty catalogs are valid for a new installation. TOML contains
infrastructure settings such as `credential_stores` and services.

## LLM accounts

`/api/v1/llm-accounts` supports GET (list) and POST (create).
`/api/v1/llm-accounts/{id}` supports GET, PATCH and DELETE.

An API-key account:

```json
{"name":"Research budget","harness":"codex","auth_mode":"api_key","api_key":"your-key"}
```

A file account uses `auth_mode: "account"`, a `credential_store` name from
`GET /api/v1/credential-stores`, and an `auth_file` JSON object containing Codex
account authorization or an active OpenCode Console export. For OpenCode, run
`opencode auth login opencode`, then prepare the file in a private directory:

```sh
(umask 077
 set -C
 opencode auth export opencode |
   jq -e 'map(select(.active and .integrationID == "opencode" and .value.type == "oauth" and .value.methodID == "device")) | if length == 1 then .[0] else error("Expected one active Console credential") end' > opencode-account.json
)
```

Upload the resulting JSON object in the account editor.
The API generates `llm-accounts/<uuid>/auth.json` in S3;
clients do not choose paths. Credential store configuration and AWS access must
be available to both API and worker.

For an OpenCode key, specify the integration ID in `provider`:

```json
{"name":"Kimi plan","harness":"opencode","provider":"kimi-code-plan-global","auth_mode":"api_key","api_key":"your-key"}
```

A custom provider ID must match a provider configured in the OpenCode template.
The provider is immutable. Codex accounts default to `openai`. OpenCode Console
file accounts use `opencode`.

PATCH accepts `name` and `active`. Display names need not be unique. Harness,
authorization mode and file location are immutable. Accounts default to active.
An account cannot be deleted while any profile or session references it. Deletion
removes its encrypted API key in the same transaction and leaves S3 files intact.

API keys are encrypted in `secrets` using `ENV_ENCRYPTION_KEY`. Only account
details include `api_key_mask`; lists never include it. PUT
`/api/v1/llm-accounts/{id}/api-key` replaces the key with `{"api_key":"new-key"}`.
Keys cannot be revealed. Whitespace is preserved in storage; only the mask trims it.

GET `/api/v1/llm-accounts/{id}/auth-file` returns the full JSON with `ETag` and
`Cache-Control: no-store`. PUT to the same path requires that ETag in `If-Match`.
A concurrent edit or worker token refresh returns 409 `auth_file_changed`; reload
before editing again. There is no forced overwrite. Unknown JSON fields survive;
the file is limited to 1 MiB, in addition to the whole-request `MAX_REQUEST_BYTES`
limit. Raise the request limit if an account creation envelope would exceed it.
Auth file contents and API keys must not be placed in names or descriptions.

## Model profiles

`/api/v1/profiles` supports GET and POST; `/api/v1/profiles/{id}` supports GET,
PATCH and DELETE. Create example:

```json
{"name":"research","account_id":"00000000-0000-0000-0000-000000000001","model":"your-model","description":"Research and analysis","codex":{"effort":"high"}}
```

The unique `name` is immutable and is the public `agent.profile` selector. `model`
is required; sessions can override it with `agent.model`. PATCH accepts account,
model, description, instructions, Codex options and active state. Rebinding to an
account of the same harness affects new sessions. Authorization modes may differ.
Null `description` clears it; null Codex option values unset individual options,
and `codex: null` clears all options. Omitted options remain unspecified.

OpenCode profiles inherit the account provider. Use the model ID from the
template or provider catalog, such as `k3.1-pro` on global Kimi Coding Plan.
For Console OAuth, use `opencode-go/<model-id>` for the Go subscription.
An unqualified Console model uses the `opencode` provider and its billing.
To set reasoning effort for an OpenCode profile, put the native variant ID in
`opencode.variant`. Keep the model ID in `model`. For example, a Kimi profile
can use `"model": "k3.1-pro", "opencode": {"variant": "max"}`.
Available variants depend on the provider and model. An omitted variant uses
the model default. PATCH with `{"opencode":{"variant":null}}` to clear it.
Sessions keep the setting captured at creation.

Codex options apply only to Codex profiles.

Responses include `account_id`, `account_name`, `account_active`, `active` and
`available` (`active && account_active`). Descriptions and instructions are not
secret fields. New sessions require an available profile and return 422
`unknown_profile`, with `profile_not_found`, `profile_inactive` or
`account_inactive` detail codes otherwise.

Admission locks the profile and account in the transaction that accepts the
session. Sessions retain execution settings and their account UUID; they do not
store credential paths or keys. Profile edits, deletion and deactivation do not
change accepted sessions. Continuation and idempotent replay do not revalidate
the profile. An account's current credentials are used when a harness is launched
or repaired; replacing credentials does not relogin an already-running process.

Both catalogs use the list parameters and bidirectional cursors described below
for templates. Profiles additionally accept `available=true|false`, including
account activity. Resolve selected names using exact `name` lookup without an
activity filter. Account lists are ordered by `(name, id)` because names may repeat.

For existing installations, follow the [catalog upgrade procedure](llm-catalog-upgrade.md).

## Database template catalog

The template catalog is stored in PostgreSQL. New sessions require an exact,
active catalog name, including any team slug and tag. Changes take effect without
restarting API or worker. An empty catalog is valid at startup and rejects new
sessions. There is no automatic import or seed.

Missing and inactive names both return 422 `unknown_template` at
`body.configuration.sandbox.template`. The detail code is `template_not_found`
or `template_inactive`, respectively. Admission does not normalize the selected
name, resolve aliases or call AgentBox. It checks and locks the catalog row in the
same transaction that accepts the session, so concurrent deletion or deactivation
cannot overtake an admission that has already selected an active record.

Accepted sessions retain their exact template name. Workers, continuation runs
and idempotent replays do not read the catalog again. Disabling or deleting a
record does not cancel queued work, change snapshots or affect existing
sandboxes. Description changes do not affect execution either.

When upgrading from a TOML catalog, register the required names before switching
versions. Remove the obsolete `[templates]` section from the new configuration;
the strict TOML parser rejects it. Coordinate the new binary and configuration
for both API and worker. Infrastructure TOML sections remain in use.

| Request | Result |
| --- | --- |
| `GET /api/v1/templates` | One page, ordered by stored name |
| `GET /api/v1/templates/{id}` | One record by its Orpheus UUID |
| `POST /api/v1/templates` | Verify in AgentBox and create a record (201) |
| `PATCH /api/v1/templates/{id}` | Change description and/or active state |
| `DELETE /api/v1/templates/{id}` | Delete only the catalog record (204) |

Create example:

```json
{"template_name":"team/orpheus-codex","tag":"v1","description":"Codex sandbox","active":true}
```

Names accept a short alias or `team/alias`; supply the optional tag separately.
Name and tag are lowercased. Build UUIDs are not tags. A missing or null tag stays
null; verification requires an existing `default` tag, without pinning a build.
Explicit `default` and null remain distinct choices. Only templates owned by the
configured AgentBox project can be registered. Verification checks existence,
not build readiness, and does not start a sandbox.

The API process needs the same `AGENTBOX_API_KEY` and endpoint configuration as
the worker. `AGENTBOX_API_URL` takes precedence over `AGENTBOX_DEBUG=true`
(`http://localhost:3000`), then `AGENTBOX_DOMAIN` (default `agentbox-runtime.ru`,
API origin `https://api.<domain>`). Invalid endpoint configuration fails startup.
A missing/invalid key or unavailable AgentBox returns 503. Reads, updates and
deletes do not contact AgentBox.

Records contain `id`, `name` (`template_name[:tag]`), `template_name`, nullable
`tag` and `description`, and `active`. The provider ID is internal. Active defaults
to true in the API and database. Duplicate names or provider ID/tag pairs return
409, including inactive records. PATCH permits only `active` and `description`;
empty or unknown-field patches and `active: null` return 422. Null, empty and
whitespace-only descriptions clear the value. Missing records return 404;
malformed UUIDs return 422.

List parameters:

- `active=true|false`: omit for both states.
- `name`: exact stored name, case-sensitive, including any team slug and tag.
- `name_contains`: literal case-insensitive substring; `%`, `_` and `\` are literals.
- `limit`: 1–200, default 50.
- `cursor`: a returned `next_cursor` or `prev_cursor` with the same filters.

`name` and `name_contains` cannot be combined. Blank or NUL-containing searches
return 422. Responses contain `items`, `next_cursor` and `prev_cursor`; unavailable
directions are null. Both directions return ascending names. Invalid or
filter-mismatched cursors return 422 `invalid_cursor`; restart from the first
page explicitly. Pages reflect concurrent changes; even an empty page may have
a cursor back to existing records. To resolve a selected name, use `name`
without an active filter rather than scanning pages.

Browser writes use the shared [Origin/CSRF rules](browser-auth.md); bearer keys
remain supported. Mutation logs record action, record ID, actor, field names and
status, without descriptions or credentials. Migration 29 requires PostgreSQL 16
and permission to install `pg_trgm`; rollback drops the table but retains the
shared extension.

## Codex settings

Optional Codex settings belong in the profile's `codex` object:

```json
{"effort":"medium","summary":"auto","personality":"friendly","service_tier":"default"}
```

| Setting | Values |
| --- | --- |
| `effort` | `none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`, `ultra` |
| `summary` | `auto`, `concise`, `detailed`, `none` |
| `personality` | `none`, `friendly`, `pragmatic` |
| `service_tier` | A nonblank tier name accepted by the Codex provider, such as `default` |

Each setting is optional; omitted settings are not sent to Codex. Explicit empty
values are rejected. Model and account support still determine which options
Codex can use. Settings are captured when a session is created and reused after
resume; changing the profile affects new sessions. They are profile settings,
not public API overrides. Connectors select them through `agent.profile`.
