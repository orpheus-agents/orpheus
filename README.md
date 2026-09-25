<p align="center">
  <a href="https://github.com/orpheus-agents/orpheus">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset=".github/orpheus-logo.svg">
      <img src=".github/orpheus-logo-light.svg" alt="Orpheus" width="240">
    </picture>
  </a>
</p>

# Orpheus

The orchestrator of AI agents working in [AgentBox](https://agentbox.ru) sandboxes.
Implemented in Go 1.27 with PostgreSQL 16.

## Quick start

Requirements: Docker with Compose v2.24 or newer, and Make.

```sh
cp .env.dist .env
cp orpheus.toml.dist orpheus.toml
```

Set `AGENTBOX_API_KEY` and `OPENAI_API_KEY` in `.env`.
Add `model = "your-model"` under `[profiles.default]` in `orpheus.toml`.
Optionally set `effort = "medium"` under `[profiles.default.codex]`. If omitted,
Codex uses the model default.

```sh
make start
curl -fsS http://localhost:8000/ready
```

The API is available at `http://localhost:8000`. By default, session requests require
`Authorization: Bearer <key>` using a key from `PUBLIC_API_KEYS` in `.env`.
For read-only browser access, see [browser authentication](docs/browser-auth.md).
For the dashboard snapshot and session filters, see [dashboard analytics](docs/dashboard-analytics.md).
For provider quota snapshots by account, see [account limits](docs/account-limits.md).
Run `make stop` to stop the project; database data is preserved.

Run and hook deadlines are independent of the sandbox timeout. The worker creates
and reconnects sandboxes with a five-minute lease and renews it once per minute
while preparing, executing or finalizing a run. A one-hour run does not require
requesting more than one hour from AgentBox to cover hooks and cleanup. If the
worker stops renewing, AgentBox auto-pauses the sandbox when the lease expires;
after a run, Orpheus explicitly pauses it. The AgentBox plan's maximum uninterrupted
sandbox lifetime still applies: lease renewal does not extend that limit.

## Go API client

Connectors import `github.com/orpheus-agents/orpheus/client`. The public HTTP client
and wire types are generated from `api/openapi.yaml` with the pinned `oapi-codegen`
version. No server startup, database connection or connector-side generation is needed.

The client belongs to the root Go module and uses the same `vX.Y.Z` release tag as
Orpheus. Pin a tag containing the client with
`go get github.com/orpheus-agents/orpheus/client@<release-tag>`; `v0.1.0` predates it.
See [client usage](client/README.md) for authentication, responses and SSE.

Regular CI runs `make check`, including client generation, tests and compilation.
Tag pushes run only `make build-client` before publishing the image; generation
checks and tests are not repeated on tags.
Go consumers obtain the source from the Git tag: a separate binary or package
registry upload is unnecessary. Generation happens before committing; CI rejects
stale output instead of silently generating a different release.
[Go module publishing](https://go.dev/doc/modules/publishing).

## Development

The application, generators, linters and tests run in Docker; host Go and Python
are not required.

On Linux, Make uses the host UID/GID for development containers. Override with
`ORPHEUS_UID=1000 ORPHEUS_GID=1000 make <target>` when needed; other hosts default
to `1000:1000`.

The `migrate` command reads `migrations/` relative to the container's working
directory. Use `migrate --dir /path/to/migrations up` or
`ORPHEUS_MIGRATIONS_DIR` to select another directory inside the container.

| Command | Purpose |
| --- | --- |
| `make tools` / `make tools-build` | Ensure the matching tools image exists / explicitly rebuild it |
| `make start` / `make stop` | Start or stop Compose services; preserve database volumes |
| `make migrate` | Apply Goose migrations from `migrations/` |
| `make generate` | Generate OpenAPI server, public Go client and sqlc queries/models |
| `make generate-check` | Compare all generated content, including missing/obsolete files, without changing the checkout |
| `make generate-client` / `make generate-client-check` | Generate / check only the Go client; no SQL generation |
| `make test-client` / `make build-client` | Test the client with the race detector / compile the package |
| `make sqlc-check` | Prepare all SQL queries against an isolated PostgreSQL schema created by Goose |
| `make fix` / `make gofix` | Format Go / apply Go toolchain fixes |
| `make tidy-check` | Check `go.mod` and `go.sum` with `go mod tidy -diff`, without modifying files |
| `make deadcode` | Reject unreachable Go functions, including test entrypoints and integration/live build tags |
| `make lint` | Go, OpenAPI, Dockerfile and Python reader checks |
| `make test` | Unit, reader, PostgreSQL, S3 and migration tests |
| `make test-go-race` | Unit and integration tests with the race detector |
| `make test-saml` | Signed SAML round trip through the production HTTP handler and isolated Keycloak; no UI required |
| `make test-live` | Manually run real AgentBox/Codex, lost-response recovery, reconnect and account-file checks; not run by CI |
| `make vuln` | govulncheck and Trivy vulnerability/configuration scans |
| `make build` | Compile the application inside the tools container |
| `make docker-build` / `make smoke` | Build images / verify production serve, worker, migrate and shutdown |
| `make check` | Check generation, module tidiness, Go fixes, lint, dead code, SQL, tests, race, vulnerabilities and compilation |
