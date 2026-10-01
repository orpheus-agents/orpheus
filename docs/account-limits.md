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

Each account item includes `reset_credits_available`: the last observed number
of available earned rate-limit resets, or `null` when unknown. Zero means the
provider confirmed that no resets are available. This is an account-wide count,
not a count per profile, bucket or window. It is separate from a window's
`resets_at` timestamp and from monetary credits. Orpheus does not consume resets.

The count comes from `rateLimitResetCredits.availableCount` in the documented
[`account/rateLimits/read` RPC](https://learn.chatgpt.com/docs/app-server#6-rate-limits-chatgpt).
The worker sends `{"excludeResetCreditDetails": true}` to skip the separate
detail lookup and receive the count with `credits: null`. This stable parameter
does not require experimental API. It avoids waiting for details within the
worker's three-second read deadline. The optional detail list can be capped and
is never used to calculate the count. Missing/null summaries are unknown;
malformed counts reject the entire observation. Counts are stored in the
existing JSON snapshot, so no database
migration or backfill is needed. Older snapshots return `null` until sampled.
An unavailable count in a new successful sample replaces the old count with
`null`; a failed read retains the last successful sample and its observation time.

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

The count uses the same account freshness state and `observed_at` as the windows.
It can remain visible when stale; that does not confirm that those resets are
still available. [Service metrics](service-metrics.md) export the same snapshot.
There is intentionally no continuous polling when no active donor exists.

The sanitized fixture in `tests/fixtures/codex-rate-limits-0.156.1.json` follows
the JSON schema generated from the AgentBox `codex` template on 2026-09-24.
The Go live test can verify an account without storing credentials in the repo:

```sh
ORPHEUS_TEST_ACCOUNT_AUTH_FILE=/private/path/auth.json \
  ORPHEUS_TEST_ACCOUNT_TEMPLATE=orpheus-codex \
  go test -tags live -run '^TestLiveAccountLimits$' -count=1 -v ./internal/harness/codex
```

`ORPHEUS_TEST_ACCOUNT_TEMPLATE` defaults to `codex`; select the installation's
template to check its actual binary. The test reports the Codex version and
distinguishes an absent reset field, a present null summary and a known count.
For a known count it also checks that details are null. It does not run a model
turn or consume a reset.
Run this against the target template and a test account before enabling the
collector in production. This is a manual check, not a CI job. `AGENTBOX_API_KEY`
must be set, and the sandbox must be able to reach the ChatGPT backend. The
test creates a short-lived sandbox and removes it afterward. It starts Codex
with the same `HTTPS_PROXY`/`HTTP_PROXY`/`NO_PROXY` defaults as the worker; set
`SANDBOX_PROXY_URL` to override the proxy or to an empty value to disable it.
The manual run on 2026-09-25 passed with the AgentBox `codex` template
(Codex 0.157.0) and the default HTTPS proxy. An old SOCKS5 override is rejected
at startup; update it to an HTTP(S) proxy URL before deploying this version.

On 2026-10-01, the reset-count live test passed with Codex 0.159.3 in a temporary
template built from the current `orpheus-codex` definition in a personal AgentBox
project, using a local test account and the worker's default HTTPS proxy.
The response contained a known reset count without enabling experimental API.
The repeated run with `excludeResetCreditDetails: true` returned a count of 2
and confirmed `credits: null` within the worker's three-second read deadline.
The sandbox and template were removed afterward. This verifies that build and
account, not every already-deployed production sandbox. The synthetic payload in
`tests/fixtures/codex-reset-credits-0.159.3.json` represents a count-only response
with `credits: null`. Parser tests separately cover capped detail lists.
