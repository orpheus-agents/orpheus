# Profiles and templates

Define agent profiles and allowed sandbox templates in `orpheus.toml`.
For example, a complete configuration with one profile and one template:

```toml
[profiles.research]
description = "Research and analysis"
harness = "codex"
model = "your-model"

[profiles.research.auth]
mode = "api_key"
api_key_env = "OPENAI_API_KEY"

[templates.orpheus-codex]
description = "Codex sandbox"
```

At least one template is required. Each key is the exact AgentBox template name,
including any tag; registration does not create or check the template in AgentBox.
Set optional `description` under `[profiles.<name>]` to describe a profile.
API and worker must use the same configuration file; restart them after changes.

`GET /api/v1/profiles` returns the complete profile catalog as `{"items": [...]}`,
sorted by name. Profiles expose `name`, `description`, `harness`, `model`, `codex`
and `instructions`; authentication settings and credentials are excluded.
Missing, empty or whitespace-only descriptions and missing models are `null`.
Omitted Codex options stay absent. Descriptions and instructions must not contain secrets.

New sessions still validate profile and template names against TOML and return
422 (`unknown_profile` or `unknown_template`) for missing entries. Removing a
name does not change accepted sessions or their idempotent replays. Descriptions
are not part of execution snapshots.

## Database template catalog

The template management API reads PostgreSQL. During this rollout, session
admission and workers still use TOML: registering, disabling or deleting a
catalog record does not change which names new sessions accept. Keep the TOML
entries until the separate runtime switch. There is no automatic import or seed.

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

Optional Codex settings belong under `[profiles.default.codex]`:

```toml
[profiles.default.codex]
effort = "medium"
summary = "auto"
personality = "friendly"
service_tier = "default"
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
