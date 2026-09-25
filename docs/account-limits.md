# Account limits

Orpheus reads ChatGPT rate limits from active, initialized Codex app-servers and
serves their latest snapshots at `GET /api/v1/accounts/limits`. The endpoint does
not contact Codex, read credentials, or wake a sandbox. It accepts Bearer auth
and the read-only browser modes described in [browser authentication](browser-auth.md).

Give every account profile a stable, non-secret `account_id`:

```toml
[profiles.fast.auth]
mode = "account"
account_id = "team-main"
store = "main"
key = "codex/team/auth.json"
```

Profiles sharing one account must reference the same resolved credential
source. A source cannot have two IDs. Set `account_id` on existing account
profiles **before** upgrading. New sessions retain the ID and source in their
immutable credential snapshot; older sessions continue to execute but do not
report limits. Changing the person or organization behind credentials requires
a new ID. Changes to account configuration take effect after restarting serve
and worker with the same configuration revision.

API-key-only installations return an empty `items` list. Account profiles
without a successful sample appear as `unknown` or `unavailable`, never as 0% used.

The worker polls one running donor per account about once a minute and keeps
using the last successful donor while it remains healthy. A connection failure
switches to a reserve donor without publishing an intermediate error. After two
different donors time out, disconnect, or return an invalid response, collection
pauses for the account instead of probing every session. The next attempt starts
with another donor when one is available.
Unsupported or unauthenticated connections are retried after reconnect; invalid
responses and connection failures use per-donor backoff. Provider errors and
missing data use account-wide backoff so another session does not repeat the
same failed request. After all donors stop,
the last snapshot remains available and becomes `stale` after five minutes, an
unrecovered read failure, or a passed window reset.
A stale response retains the last percentages and `observed_at`. `error_code`
is a short category; raw provider errors and credential locations are not
returned. Limits are observational: they do not block runs or change session
token budgets.

The sanitized fixture in `tests/fixtures/codex-rate-limits-0.156.1.json` follows
the JSON schema generated from the AgentBox `codex` template on 2026-09-24.
The Go live test can verify an account without storing credentials in the repo:

```sh
ORPHEUS_TEST_ACCOUNT_AUTH_FILE=/private/path/auth.json \
  go test -tags live -run '^TestLiveAccountLimits$' -count=1 -v ./internal/harness/codex
```

Run this against the target template and a test account before enabling the
collector in production. This is a manual check, not a CI job. `AGENTBOX_API_KEY`
must be set, and the sandbox must be able to reach the ChatGPT backend. The
test creates a short-lived sandbox and removes it afterward. It starts Codex
with the same `HTTPS_PROXY`/`HTTP_PROXY`/`NO_PROXY` defaults as the worker; set
`SANDBOX_PROXY_URL` to override the proxy or to an empty value to disable it.
The manual run on 2026-09-25 passed with the AgentBox `codex` template
(Codex 0.157.0) and the default HTTPS proxy. An old SOCKS5 override is rejected
at startup; update it to an HTTP(S) proxy URL before deploying this version.
