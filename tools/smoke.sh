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
cleanup() {
    docker rm -f "$api_name" "$worker_name" >/dev/null 2>&1 || true
    docker exec "$db" psql -U orpheus -d orpheus_test -c "DROP SCHEMA IF EXISTS $schema CASCADE" >/dev/null 2>&1 || true
    rm -f "$config"
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
docker exec "$db" psql -U orpheus -d orpheus_test -c "CREATE SCHEMA $schema" >/dev/null
docker run --rm orpheus:local --help
docker run --rm --network "$network" -e DATABASE_URL="$database_url" orpheus:local migrate up
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
docker stop -t 15 "$api_name" "$worker_name" >/dev/null
[ "$(docker inspect -f '{{.State.ExitCode}}' "$api_name")" = 0 ]
[ "$(docker inspect -f '{{.State.ExitCode}}' "$worker_name")" = 0 ]
