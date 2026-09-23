COMPOSE = docker compose
RUN = $(COMPOSE) --profile tools run --rm --no-deps tools
# The tools image has no local COPY inputs; its Dockerfile defines all tools.
# POSIX cksum works on macOS and Linux without requiring host Go or Python.
export ORPHEUS_UID ?= $(shell if [ "$$(uname -s)" = Linux ]; then id -u; else echo 1000; fi)
export ORPHEUS_GID ?= $(shell if [ "$$(uname -s)" = Linux ]; then id -g; else echo 1000; fi)
export ORPHEUS_TOOLS_IMAGE := orpheus-tools:$(shell cksum < .docker/tools/Dockerfile | awk '{print $$1 "-" $$2}')-$(ORPHEUS_UID)-$(ORPHEUS_GID)

.PHONY: tools tools-build start stop generate generate-check sqlc-check fix format gofix gofix-check tidy-check deadcode lint lint-go lint-api lint-docker lint-reader test test-go test-reader test-integration test-migrations test-go-race test-live vuln build docker-build migrate check

tools:
	@docker image inspect "$(ORPHEUS_TOOLS_IMAGE)" >/dev/null 2>&1 || $(COMPOSE) build tools

tools-build:
	$(COMPOSE) build tools

start: migrate
	$(COMPOSE) up -d --wait app worker

stop:
	$(COMPOSE) --profile tools --profile test down

migrate:
	$(COMPOSE) build app
	$(COMPOSE) up -d --wait db
	$(COMPOSE) run --rm --no-deps app go run ./cmd/orpheus migrate up

generate: tools
	$(RUN) go run ./tools/generate

generate-check: tools
	$(RUN) go run ./tools/generate -check

.PHONY: generate-client generate-client-check test-client
generate-client: tools
	$(RUN) go run ./tools/generate -client

generate-client-check: tools
	$(RUN) go run ./tools/generate -client -check

test-client: tools
	$(RUN) go test -race ./client

sqlc-check: tools
	$(COMPOSE) --profile test up -d --wait test-db
	$(RUN) go test -tags integration -count=1 ./tools/sqlcheck

fix format: tools
	$(RUN) sh -ec 'gofmt -w client cmd internal tools; goimports -w client cmd internal tools'

gofix: tools
	$(RUN) go fix ./...

gofix-check: tools
	$(RUN) sh -ec 'f=$$(mktemp); trap '\''rm -f "$$f"'\'' EXIT; go fix -diff ./... > "$$f"; if test -s "$$f"; then cat "$$f"; exit 1; fi'

tidy-check: tools
	$(RUN) go mod tidy -diff

# deadcode reports findings on stdout but exits successfully; fail on any finding.
# Include test entrypoints and both build tags so test-only helpers remain reachable.
# Public client methods are entrypoints for external consumers, not dead code.
deadcode: tools
	$(RUN) sh -ec 'f=$$(mktemp); trap '\''rm -f "$$f"'\'' EXIT; go tool deadcode -filter="^github.com/orpheus-agents/orpheus/(cmd|internal|tools)(/|$$)" -test -tags=integration,live ./... > "$$f"; if test -s "$$f"; then cat "$$f"; exit 1; fi'

lint-go: tools
	$(RUN) sh -ec 'files=$$(gofmt -l client cmd internal tools); if test -n "$$files"; then printf "%s\n" "$$files"; exit 1; fi'
	$(RUN) go vet ./...
	$(RUN) golangci-lint run --build-tags integration ./...
	$(RUN) golangci-lint run --build-tags live ./...

lint-api: tools
	$(RUN) redocly lint --config redocly.yaml api/openapi.yaml

lint-docker: tools
	$(RUN) hadolint .docker/app/dev/Dockerfile .docker/app/prod/Dockerfile .docker/tools/Dockerfile

lint-reader: tools
	$(RUN) ruff check internal/harness/codex/native_reader.py tests/reader
	$(RUN) ruff format --check internal/harness/codex/native_reader.py tests/reader

lint: lint-go lint-api lint-docker lint-reader

test-go: tools
	$(RUN) go test ./...

test-reader: tools
	$(RUN) python3 -m unittest discover -s tests/reader

test-integration: tools
	$(COMPOSE) --profile test up -d --wait test-db test-s3
	$(RUN) go test -tags integration ./internal/... ./cmd/...

test-migrations: tools
	$(COMPOSE) --profile test up -d --wait test-db
	$(RUN) go test -tags integration ./internal/migrate

test: test-go test-reader test-integration test-migrations

test-go-race: tools
	$(COMPOSE) --profile test up -d --wait test-db test-s3
	$(RUN) go test -race -tags integration ./...

test-live: tools
	$(COMPOSE) --profile test up -d --wait test-db test-s3
	$(COMPOSE) --profile tools run --rm --no-deps -e AGENTBOX_API_KEY -e OPENAI_API_KEY -e ORPHEUS_TEST_MODEL tools go test -tags live -count=1 -timeout=15m ./...

vuln: tools
	$(RUN) govulncheck ./...
	$(RUN) trivy fs --no-progress --db-repository ghcr.io/aquasecurity/trivy-db:2 --db-repository mirror.gcr.io/aquasec/trivy-db:2 --scanners vuln,misconfig --exit-code 1 --severity HIGH,CRITICAL --skip-dirs .git --skip-dirs bin .

build: tools
	$(RUN) go build -trimpath -o /tmp/orpheus ./cmd/orpheus

.PHONY: build-client
build-client: tools
	$(RUN) go build ./client

docker-build:
	$(COMPOSE) build app
	docker build -f .docker/app/prod/Dockerfile -t orpheus:local .

check: generate-check
	$(MAKE) tidy-check
	$(MAKE) gofix-check
	$(MAKE) lint
	$(MAKE) deadcode
	$(MAKE) sqlc-check
	$(MAKE) test
	$(MAKE) test-go-race
	$(MAKE) vuln
	$(MAKE) build
	$(MAKE) build-client

.PHONY: smoke
smoke: docker-build
	$(COMPOSE) --profile test up -d --wait test-db
	sh tools/smoke.sh
