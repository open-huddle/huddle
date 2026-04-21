.PHONY: help install proto proto-lint proto-breaking api-run api-build api-test api-lint web-run web-build web-lint web-typecheck dev-up dev-down lint test tidy fmt ent-generate migrate-diff migrate-apply migrate-status

# Postgres image used by both the local compose stack and the throwaway
# container that Atlas/Ent uses to compute schema diffs. Bumping here bumps
# both so diffs stay on the same major version as the DB that actually runs.
POSTGRES_IMAGE ?= postgres:18.3-alpine
export POSTGRES_IMAGE

# Applied-to URL for `atlas migrate apply`. Defaults match deploy/compose.
HUDDLE_DATABASE_URL ?= postgres://huddle:huddle@localhost:5432/huddle?sslmode=disable

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

install: ## Install all workspace dependencies (pnpm + go)
	pnpm install
	go work sync

proto: ## Generate Go + TS code from .proto files
	buf generate

proto-lint: ## Lint .proto files
	buf lint

proto-breaking: ## Detect breaking changes in .proto files (against main)
	buf breaking --against '.git#branch=main'

api-run: ## Run the Go API locally
	cd apps/api && go run ./cmd/api

api-build: ## Build the Go API binary
	cd apps/api && go build -o bin/api ./cmd/api

api-test: ## Run Go tests
	cd apps/api && go test ./...

api-lint: ## Lint Go code (requires golangci-lint)
	cd apps/api && golangci-lint run ./...

web-run: ## Run the web app in dev mode
	pnpm --filter @open-huddle/web dev

web-build: ## Build the web app
	pnpm --filter @open-huddle/web build

web-typecheck: ## Typecheck the web app
	pnpm --filter @open-huddle/web typecheck

web-lint: ## Lint the web app
	pnpm --filter @open-huddle/web lint

dev-up: ## Start local dependencies (postgres, valkey)
	docker compose -f deploy/compose/docker-compose.yml up -d

dev-down: ## Stop local dependencies
	docker compose -f deploy/compose/docker-compose.yml down

lint: api-lint web-lint proto-lint ## Run all linters

test: api-test ## Run all tests

tidy: ## go mod tidy across the workspace
	cd apps/api && go mod tidy
	cd gen/go && go mod tidy

fmt: ## Format Go + TS code
	cd apps/api && go fmt ./...
	pnpm --filter @open-huddle/web format

ent-generate: ## Regenerate Ent code from apps/api/ent/schema
	cd apps/api && go tool ent generate --feature sql/versioned-migration --feature sql/upsert ./ent/schema

migrate-diff: ## Generate an Atlas migration from Ent schema changes (NAME=desc)
	@if [ -z "$(NAME)" ]; then echo "NAME is required (e.g., make migrate-diff NAME=add_channels)"; exit 1; fi
	cd apps/api && ./scripts/migrate-diff.sh $(NAME)

migrate-apply: ## Apply pending migrations to HUDDLE_DATABASE_URL (default: local compose)
	cd apps/api && atlas migrate apply --env local --var db_url="$(HUDDLE_DATABASE_URL)"

migrate-status: ## Show Atlas migration status against HUDDLE_DATABASE_URL
	cd apps/api && atlas migrate status --env local --var db_url="$(HUDDLE_DATABASE_URL)"
