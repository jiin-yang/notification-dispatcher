.DEFAULT_GOAL := help

-include .env
export

GOOSE_DRIVER  := postgres
GOOSE_DBSTRING := $(DB_DSN)
MIGRATIONS_DIR := migrations

# Derive test DSN by replacing the database name segment in DB_DSN.
# e.g. postgres://notifier:notifier@localhost:5432/notifications?sslmode=disable
#   -> postgres://notifier:notifier@localhost:5432/notifications_test?sslmode=disable
TEST_DB_DSN := $(shell echo "$(DB_DSN)" | sed 's|/notifications?|/notifications_test?|')

POSTGRES_CONTAINER := notification-dispatcher-postgres-1

.PHONY: help
help:
	@echo "Usage: make <target>"
	@echo ""
	@echo "Infrastructure:"
	@echo "  dev-up              Start RabbitMQ and Postgres in the background (waits for healthy)"
	@echo "  dev-down            Stop and remove containers (volumes are preserved)"
	@echo "  dev-clean           Stop containers AND delete volumes  *** destroys all local data ***"
	@echo "  dev-logs            Tail all container logs"
	@echo "  dev-ps              Show container status"
	@echo ""
	@echo "Migrations (requires goose: go install github.com/pressly/goose/v3/cmd/goose@latest):"
	@echo "  migrate-up          Apply all pending migrations"
	@echo "  migrate-down        Roll back the most recent migration"
	@echo "  migrate-status      Show migration status"
	@echo "  migrate-create      Create a new SQL migration file  usage: make migrate-create name=<name>"
	@echo ""
	@echo "App:"
	@echo "  build               Compile api and worker binaries into bin/"
	@echo "  run-api             Run the API server via go run"
	@echo "  run-worker          Run the worker via go run"
	@echo "  lint                Run golangci-lint (requires: https://golangci-lint.run/usage/install/)"
	@echo "  tidy                Run go mod tidy"
	@echo ""
	@echo "Tests:"
	@echo "  test                Alias for test-unit (back-compat)"
	@echo "  test-unit           Run unit tests (no external deps)"
	@echo "  test-e2e            Bring up test DB, apply migrations, run e2e tests (requires dev-up)"
	@echo "  test-all            Run test-unit then test-e2e (used by pre-push hook)"
	@echo "  test-e2e-cleanup    Drop the notifications_test database"
	@echo ""
	@echo "Git hooks:"
	@echo "  hooks-install       Configure git to use .githooks/ (run once per clone)"

.PHONY: dev-up
dev-up:
	docker compose up -d --wait

.PHONY: dev-down
dev-down:
	docker compose down

.PHONY: dev-clean
dev-clean:
	docker compose down -v

.PHONY: dev-logs
dev-logs:
	docker compose logs -f

.PHONY: dev-ps
dev-ps:
	docker compose ps

.PHONY: migrate-up
migrate-up:
	goose -dir $(MIGRATIONS_DIR) up

.PHONY: migrate-down
migrate-down:
	goose -dir $(MIGRATIONS_DIR) down

.PHONY: migrate-status
migrate-status:
	goose -dir $(MIGRATIONS_DIR) status

.PHONY: migrate-create
migrate-create:
	goose -dir $(MIGRATIONS_DIR) create $(name) sql

.PHONY: build
build:
	go build -o bin/api    ./cmd/api
	go build -o bin/worker ./cmd/worker

.PHONY: run-api
run-api:
	go run ./cmd/api

.PHONY: run-worker
run-worker:
	go run ./cmd/worker

.PHONY: test
test: test-unit

.PHONY: test-unit
test-unit:
	go test ./...

.PHONY: test-e2e
test-e2e:
	@echo "[test-e2e] Checking that Postgres and RabbitMQ containers are healthy..."
	@STATUS=$$(docker compose ps --format json 2>/dev/null | \
	  tr ',' '\n' | grep -c '"Health":"healthy"' || true); \
	if [ "$$STATUS" -lt 2 ]; then \
	  echo "[test-e2e] ERROR: one or more containers are not healthy."; \
	  echo "  Run 'make dev-up' first, then retry."; \
	  exit 1; \
	fi
	@echo "[test-e2e] Creating notifications_test database if it does not exist..."
	@docker exec $(POSTGRES_CONTAINER) \
	  psql -U notifier -d postgres -tc \
	  "SELECT 1 FROM pg_database WHERE datname = 'notifications_test'" \
	  | grep -q 1 || \
	  docker exec $(POSTGRES_CONTAINER) \
	  psql -U notifier -d postgres -c "CREATE DATABASE notifications_test"
	@echo "[test-e2e] Applying migrations to notifications_test..."
	GOOSE_DRIVER=$(GOOSE_DRIVER) GOOSE_DBSTRING="$(TEST_DB_DSN)" \
	  goose -dir $(MIGRATIONS_DIR) up
	@echo "[test-e2e] Running e2e tests..."
	TEST_DB_DSN="$(TEST_DB_DSN)" TEST_RMQ_URL="$(RMQ_URL)" \
	  go test -tags=e2e -count=1 -timeout 120s ./test/e2e/...

.PHONY: test-all
test-all: test-unit test-e2e

.PHONY: test-e2e-cleanup
test-e2e-cleanup:
	@echo "[test-e2e-cleanup] Dropping notifications_test database..."
	docker exec $(POSTGRES_CONTAINER) \
	  psql -U notifier -d postgres -c "DROP DATABASE IF EXISTS notifications_test"
	@echo "[test-e2e-cleanup] Done."

.PHONY: hooks-install
hooks-install:
	git config core.hooksPath .githooks
	@echo "[hooks-install] Git hooks path set to .githooks/"

.PHONY: lint
lint:
	golangci-lint run

.PHONY: tidy
tidy:
	go mod tidy
