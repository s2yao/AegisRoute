# AegisRoute Makefile — final target list (stubs are replaced in the stage
# where their artifact lands).
#
# Config reads only os.Getenv (no dotenv library), so host-infra targets —
# once implemented — must load .env first by prefixing their recipes with:
#   set -a; [ -f .env ] && . ./.env; set +a; <command>

SHELL := /bin/sh

.DEFAULT_GOAL := help

.PHONY: help fmt vet test verify test-integration migrate-up seed-dev \
	dev-up dev-down logs verify-e2e clean

help: ## List all targets with descriptions
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  %-18s %s\n", $$1, $$2}'

fmt: ## Format all Go source with gofmt
	gofmt -w .

vet: ## Run go vet on all packages
	go vet ./...

test: ## Run unit tests (no Docker/Postgres/Redis required)
	go test ./...

verify: ## Gate: gofmt clean, then go vet, then go test (no Docker)
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt required for:"; echo "$$unformatted"; exit 1; \
	fi
	go vet ./...
	go test ./...

test-integration: ## Run //go:build integration tests against real Postgres/Redis
	set -a; [ -f .env ] && . ./.env; set +a; go test -tags integration -count=1 ./...

migrate-up: ## Apply embedded DB migrations to DATABASE_URL
	set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/gateway-api -migrate

seed-dev: ## Seed demo tenant, API key, and backends
	set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/gateway-api -seed

dev-up: ## Start the full local stack via docker compose (build + detach)
	docker compose up -d --build

dev-down: ## Stop the local stack and remove its volumes/orphans
	docker compose down -v --remove-orphans

logs: ## Follow logs from all compose services
	docker compose logs -f

verify-e2e: ## Full end-to-end verification against a fresh compose stack
	bash scripts/e2e.sh

clean: ## Remove build/test/coverage artifacts (never source)
	go clean ./...
	rm -rf bin dist
	rm -f coverage.out coverage.html *.test *.prof
