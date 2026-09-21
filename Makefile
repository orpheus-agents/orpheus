COMPOSE = docker compose
RUN = $(COMPOSE) run --rm --no-deps app

.PHONY: start stop lint format migrate openapi test test-live

start: migrate
	$(COMPOSE) up -d --wait app worker

stop:
	$(COMPOSE) --profile test down

lint:
	$(COMPOSE) build app
	$(RUN) ruff check .
	$(RUN) ruff format --check .
	$(RUN) ty check --python /opt/venv/bin/python

format:
	$(COMPOSE) build app
	$(RUN) ruff check --fix .
	$(RUN) ruff format .

migrate:
	$(COMPOSE) build app
	$(COMPOSE) up -d --wait db
	$(RUN) alembic upgrade head

openapi:
	$(COMPOSE) build app
	$(RUN) orpheus openapi

test:
	$(COMPOSE) build app
	$(COMPOSE) --profile test up -d --wait test-db
	$(COMPOSE) run --rm --no-deps -e TEST_DATABASE_URL=postgresql+psycopg://orpheus:orpheus@test-db:5432/orpheus_test app pytest -m "not live"

test-live:
	$(COMPOSE) build app
	$(COMPOSE) --profile test up -d --wait test-db test-s3
	$(COMPOSE) run --rm --no-deps -e AGENTBOX_API_KEY -e OPENAI_API_KEY -e TEST_S3_ENDPOINT=http://test-s3:9000 -e TEST_DATABASE_URL=postgresql+psycopg://orpheus:orpheus@test-db:5432/orpheus_test app pytest -m live
