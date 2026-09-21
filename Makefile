COMPOSE = docker compose
RUN = $(COMPOSE) run --rm --no-deps app

.PHONY: start stop lint format migrate openapi test

start: migrate
	$(COMPOSE) up -d --wait app

stop:
	$(COMPOSE) down

lint:
	$(COMPOSE) build app
	$(RUN) ruff check .
	$(RUN) ruff format --check .
	$(RUN) ty check

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
	$(RUN) pytest
