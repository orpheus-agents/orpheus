#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
db=$(docker compose ps -q test-db)
network=$(docker inspect -f '{{range $name, $config := .NetworkSettings.Networks}}{{$name}}{{end}}' "$db")
api_name="orpheus-smoke-api-$$"
worker_name="orpheus-smoke-worker-$$"
schema="smoke_$$"
database_url="postgres://orpheus:orpheus@test-db:5432/orpheus_test?search_path=$schema"
config=$(mktemp)
logs=$(mktemp -d)
cleanup() {
    docker rm -f "$api_name" "$worker_name" >/dev/null 2>&1 || true
    docker exec "$db" psql -U orpheus -d orpheus_test -c "DROP SCHEMA IF EXISTS $schema CASCADE" >/dev/null 2>&1 || true
    rm -f "$config"
    rm -rf "$logs"
}
trap cleanup EXIT INT TERM
cat > "$config" <<'TOML'
[profiles.default]
harness = "codex"
model = "fixture"
[profiles.default.auth]
mode = "api_key"
api_key_env = "OPENAI_API_KEY"
TOML
chmod 644 "$config"
chmod 755 "$logs"
docker exec "$db" psql -U orpheus -d orpheus_test -c "CREATE SCHEMA $schema" >/dev/null
docker run --rm orpheus:local --help
docker run --rm --network "$network" -e DATABASE_URL="$database_url" orpheus:local migrate up \
    >"$logs/migrate.stdout" 2>"$logs/migrate.stderr"
code=0
docker run --rm orpheus:local serve --private-token \
    >"$logs/startup-error.stdout" 2>"$logs/startup-error.stderr" || code=$?
[ "$code" = 1 ]
mkdir "$logs/migrations"
cat > "$logs/migrations/99999_bad.sql" <<'SQL'
-- +goose Up
SELECT 'smoke-secret'::integer;
-- +goose Down
SELECT 1;
SQL
chmod 755 "$logs/migrations"
chmod 644 "$logs/migrations/99999_bad.sql"
code=0
docker run --rm --network "$network" -e DATABASE_URL="$database_url" \
    -v "$logs/migrations:/bad-migrations:ro" orpheus:local migrate --dir /bad-migrations up \
    >"$logs/migration-error.stdout" 2>"$logs/migration-error.stderr" || code=$?
[ "$code" = 1 ]
for command in serve worker; do
    name=$api_name
    if [ "$command" = worker ]; then name=$worker_name; fi
    docker run -d --name "$name" --network "$network" \
        -v "$config:/etc/orpheus.toml:ro" \
        -e ORPHEUS_CONFIG_FILE=/etc/orpheus.toml \
        -e DATABASE_URL="$database_url" \
        -e 'PUBLIC_API_KEYS=["smoke"]' \
        -e ENV_ENCRYPTION_KEY=Zm9yLWxvY2FsLWRldmVsb3BtZW50LW9ubHktMzJieXQ= \
        -e AGENTBOX_API_KEY=smoke \
        orpheus:local "$command" >/dev/null
 done
ready=false
for attempt in 1 2 3 4 5 6 7 8 9 10; do
    if docker exec "$api_name" /orpheus healthcheck; then ready=true; break; fi
    sleep 1
done
[ "$ready" = true ]
[ "$(docker inspect -f '{{.State.Running}}' "$worker_name")" = true ]
docker compose --profile tools run --rm --no-deps -T \
    -e SMOKE_API_URL="http://$api_name:8000" \
    -e SMOKE_SYSTEM_URL="http://$api_name:9100" tools python3 - <<'PY'
import json
import os
import urllib.error
import urllib.request

api = os.environ["SMOKE_API_URL"]
system = os.environ["SMOKE_SYSTEM_URL"]
for base, path, expected in (
    (system, "/health", 200),
    (system, "/ready", 200),
    (system, "/api/v1/sessions", 404),
    (system, "/openapi.json", 404),
    (api, "/health", 404),
    (api, "/ready", 404),
    (api, "/api/v1/sessions", 200),
):
    request = urllib.request.Request(base + path, headers={"Authorization": "Bearer smoke"})
    try:
        response = urllib.request.urlopen(request, timeout=5)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        assert response.status == expected, (base, path, response.status)
        if expected == 200:
            json.load(response)
print("System probes and authenticated API work on separate ports.")
PY
docker stop -t 15 "$api_name" "$worker_name" >/dev/null
[ "$(docker inspect -f '{{.State.ExitCode}}' "$api_name")" = 0 ]
[ "$(docker inspect -f '{{.State.ExitCode}}' "$worker_name")" = 0 ]
docker logs "$api_name" >"$logs/api.stdout" 2>"$logs/api.stderr"
docker logs "$worker_name" >"$logs/worker.stdout" 2>"$logs/worker.stderr"
chmod 644 "$logs"/*.stdout "$logs"/*.stderr
docker compose --profile tools run --rm --no-deps -T \
    -v "$logs:/tmp/smoke-logs:ro" tools python3 - <<'PY'
import json
from pathlib import Path

logs = Path("/tmp/smoke-logs")
for name, expected in (
    ("api", "HTTP servers stopping"),
    ("worker", "Worker stopped"),
    ("migrate", "Database migration completed"),
    ("startup-error", "Command failed"),
    ("migration-error", "Command failed"),
):
    assert not (logs / f"{name}.stderr").read_bytes(), name
    output = (logs / f"{name}.stdout").read_text()
    assert "private-token" not in output and "smoke-secret" not in output, name
    records = [json.loads(line) for line in output.splitlines()]
    assert records, name
    for record in records:
        assert isinstance(record["time"], str) and record["time"], record
        assert record["level"] in ("INFO", "WARN", "ERROR"), record
        assert isinstance(record["msg"], str), record
    assert any(record["msg"] == expected for record in records), (name, records)
    if name.endswith("-error"):
        assert records[-1]["level"] == "ERROR", (name, records)
    if name == "migration-error":
        assert "22P02" in records[-1]["error"], records
        assert any(record["msg"] == "Migration failed" and record["version"] == 99999 for record in records), records
        assert not any(record["msg"] == "Database migration completed" for record in records)
    if name == "migrate":
        applied = [record["version"] for record in records if record["msg"] == "Migration completed"]
        assert applied and records[-1]["current_version"] == max(applied), records
print("API, worker, migrations and startup errors emit JSON to stdout; stderr is empty.")
PY
