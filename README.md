# Orpheus

The orchestrator of AI-agents working in sandboxes (powered by [AgentBox](https://agentbox.ru)).

## Development

Requirements: Docker with Compose v2 or newer, and Make. No host Python environment is required.

```sh
cp .env.dist .env
cp orpheus.toml.dist orpheus.toml
make start
```

The service is available at `http://localhost:8000`. Source changes trigger
Uvicorn reload. Dependencies live in `/opt/venv` inside the image, outside the
source bind mount. PostgreSQL 16 runs on the internal Compose network and keeps
data in a named volume.

| Command | Purpose |
| --- | --- |
| `make start` | Build, wait for PostgreSQL, migrate, and start the HTTP service and worker in the background |
| `make stop` | Remove Compose containers and network, preserving PostgreSQL data |
| `make lint` | Run Ruff checks, format checks, and ty without modifying source files |
| `make format` | Apply Ruff fixes and formatting |
| `make migrate` | Build, start PostgreSQL, and apply Alembic migrations |
| `make openapi` | Export the API contract to `openapi.json` without starting the service or database |
| `make test` | Run tests without the `live` marker, using an isolated PostgreSQL 16 database |
| `make test-live` | Run tests marked `live` against AgentBox, OpenAI, and a local S3 service |

Make commands build the development image so tools and dependencies match the
lockfile, including on a fresh checkout. Ruff and ty use their default rules.
Build layers are cached. To inspect a running service:

```sh
docker compose logs -f app worker
docker compose exec app orpheus --help
docker compose exec app orpheus serve --help
```

## Configuration and CLI

Compose loads application environment variables from `.env`.
Available settings and their defaults are documented in [.env.dist](.env.dist).
