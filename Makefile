.DEFAULT_GOAL := help

-include .env
export

GOOSE_DRIVER  := postgres
GOOSE_DBSTRING := $(DB_DSN)
MIGRATIONS_DIR := migrations

.PHONY: help
help:
	@echo "Usage: make <target>"
	@echo ""
	@echo "Infrastructure:"
	@echo "  dev-up          Start RabbitMQ and Postgres in the background (waits for healthy)"
	@echo "  dev-down        Stop and remove containers (volumes are preserved)"
	@echo "  dev-clean       Stop containers AND delete volumes  *** destroys all local data ***"
	@echo "  dev-logs        Tail all container logs"
	@echo "  dev-ps          Show container status"
	@echo ""
	@echo "Migrations (requires goose: go install github.com/pressly/goose/v3/cmd/goose@latest):"
	@echo "  migrate-up      Apply all pending migrations"
	@echo "  migrate-down    Roll back the most recent migration"
	@echo "  migrate-status  Show migration status"
	@echo "  migrate-create  Create a new SQL migration file  usage: make migrate-create name=<name>"
	@echo ""
	@echo "App:"
	@echo "  build           Compile api and worker binaries into bin/"
	@echo "  run-api         Run the API server via go run"
	@echo "  run-worker      Run the worker via go run"
	@echo "  test            Run all tests"
	@echo "  lint            Run golangci-lint (requires: https://golangci-lint.run/usage/install/)"
	@echo "  tidy            Run go mod tidy"

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
	goose -dir $(MIGRATIONS_DIR) $(GOOSE_DRIVER) "$(GOOSE_DBSTRING)" up

.PHONY: migrate-down
migrate-down:
	goose -dir $(MIGRATIONS_DIR) $(GOOSE_DRIVER) "$(GOOSE_DBSTRING)" down

.PHONY: migrate-status
migrate-status:
	goose -dir $(MIGRATIONS_DIR) $(GOOSE_DRIVER) "$(GOOSE_DBSTRING)" status

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
test:
	go test ./...

.PHONY: lint
lint:
	golangci-lint run

.PHONY: tidy
tidy:
	go mod tidy
