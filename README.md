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

```sh
make start
curl -fsS http://localhost:8000/ready
```

The API is available at `http://localhost:8000`. Session requests require
`Authorization: Bearer <key>` using a key from `PUBLIC_API_KEYS` in `.env`.
Run `make stop` to stop the project; database data is preserved.

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
| `make generate` | Generate OpenAPI transport code and sqlc queries/models |
| `make generate-check` | Compare all generated content, including missing/obsolete files, without changing the checkout |
| `make sqlc-check` | Prepare all SQL queries against an isolated PostgreSQL schema created by Goose |
| `make fix` / `make gofix` | Format Go / apply Go toolchain fixes |
| `make tidy-check` | Check `go.mod` and `go.sum` with `go mod tidy -diff`, without modifying files |
| `make deadcode` | Reject unreachable Go functions, including test entrypoints and integration/live build tags |
| `make lint` | Go, OpenAPI, Dockerfile and Python reader checks |
| `make test` | Unit, reader, PostgreSQL, S3 and migration tests |
| `make test-go-race` | Unit and integration tests with the race detector |
| `make test-live` | Real AgentBox/Codex, lost-response recovery, reconnect and account-file checks |
| `make vuln` | govulncheck and Trivy vulnerability/configuration scans |
| `make build` | Compile the application inside the tools container |
| `make docker-build` / `make smoke` | Build images / verify production serve, worker, migrate and shutdown |
| `make check` | Check generation, module tidiness, Go fixes, lint, dead code, SQL, tests, race, vulnerabilities and compilation |
