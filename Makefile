# Wallet & Ledger Service - daily commands
#
# Every target that touches the database reads DATABASE_URL from the environment
# (see .env.example). To start a local PostgreSQL: make db-up

SHELL := /bin/bash
BINARY := bin/wallet-api
PKG := ./...

DATABASE_URL ?= postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable
TEST_DATABASE_URL ?= postgres://wallet:wallet@localhost:5432/wallet_test?sslmode=disable
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: help
help: ## Show this list of targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

## ---------------------------------------------------------------- build & run

.PHONY: build
build: ## Build the binary into bin/wallet-api
	go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o $(BINARY) ./cmd/api

.PHONY: run
run: ## Run the service (requires DATABASE_URL and a running PostgreSQL)
	DATABASE_URL="$(DATABASE_URL)" MIGRATE_ON_START=true go run ./cmd/api

## ---------------------------------------------------------------------- quality

.PHONY: fmt
fmt: ## Format the code
	gofmt -l -w .

.PHONY: vet
vet: ## go vet (including the integration-tagged files)
	go vet ./...
	go vet -tags=integration ./...

.PHONY: tidy
tidy: ## Sync go.mod/go.sum
	go mod tidy

.PHONY: lint
lint: fmt vet ## Format + vet

## ----------------------------------------------------------------------- test

.PHONY: test
test: ## Unit tests (no database required)
	go test -count=1 $(PKG)

.PHONY: test-race
test-race: ## Unit tests with the race detector
	go test -race -count=1 $(PKG)

.PHONY: cover
cover: ## Unit test coverage
	go test -coverprofile=coverage.out $(PKG)
	go tool cover -func=coverage.out | tail -1

.PHONY: test-integration
test-integration: ## Integration tests against PostgreSQL (requires make db-up)
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" \
		go test -tags=integration -race -count=1 ./internal/repository/postgres/...

.PHONY: test-all
test-all: test-race test-integration ## All tests

## ------------------------------------------------------------------ migration

.PHONY: migrate-up
migrate-up: ## Apply every migration
	DATABASE_URL="$(DATABASE_URL)" go run ./cmd/api migrate up

.PHONY: migrate-down
migrate-down: ## Roll back every migration (drops the schema)
	DATABASE_URL="$(DATABASE_URL)" go run ./cmd/api migrate down

.PHONY: migrate-version
migrate-version: ## Print the current migration version
	DATABASE_URL="$(DATABASE_URL)" go run ./cmd/api migrate version

## --------------------------------------------------------------------- docker

.PHONY: db-up
db-up: ## Start PostgreSQL only (for local development)
	docker compose up -d postgres

.PHONY: db-down
db-down: ## Stop PostgreSQL
	docker compose stop postgres

.PHONY: up
up: ## Start the whole stack (api + postgres)
	docker compose up -d --build

.PHONY: down
down: ## Stop the whole stack
	docker compose down

.PHONY: reset
reset: ## Stop the stack and delete the database volume (data is lost)
	docker compose down -v

.PHONY: logs
logs: ## Follow the api service logs
	docker compose logs -f api

## ------------------------------------------------------------------------ e2e

.PHONY: e2e
e2e: ## Run the end-to-end scenario with curl
	./scripts/e2e.sh
