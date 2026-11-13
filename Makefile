.PHONY: help build test test-unit test-docker race cover lint fuzz run up down merchant

GO ?= go
LINT_VERSION ?= v2.15.0

help: ## Show targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "%-12s %s\n", $$1, $$2}'

build: ## Build the gateway binary into bin/
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/gateway ./cmd/gateway

test-unit: ## Unit tests only (integration tests skip without DATABASE_URL)
	$(GO) test ./...

test: ## All tests with the race detector (needs DATABASE_URL)
	REQUIRE_DB=1 $(GO) test -race -count=1 ./...

test-docker: ## All tests with only Docker installed (throwaway Postgres)
	scripts/test-in-docker.sh -race -count=1 ./...

cover: ## Coverage report (needs DATABASE_URL)
	REQUIRE_DB=1 $(GO) test -race -covermode=atomic -coverpkg=./internal/...,./migrations/... -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

fuzz: ## Run each fuzz target for 20s
	$(GO) test ./internal/money -run '^$$' -fuzz FuzzParse -fuzztime 20s
	$(GO) test ./internal/money -run '^$$' -fuzz FuzzFormat -fuzztime 20s
	$(GO) test ./internal/card -run '^$$' -fuzz FuzzLuhn -fuzztime 20s
	$(GO) test ./internal/card -run '^$$' -fuzz FuzzNormalize -fuzztime 20s
	$(GO) test ./internal/pix -run '^$$' -fuzz FuzzParse -fuzztime 20s

lint: ## golangci-lint in Docker (same version as CI)
	docker run --rm -v "$(CURDIR)":/src -w /src golangci/golangci-lint:$(LINT_VERSION) golangci-lint run ./...

up: ## Start postgres, gateway and webhook receiver
	docker compose up -d --build

down: ## Stop the stack and delete its data
	docker compose down -v

merchant: ## Create a demo merchant (prints the API key once)
	docker compose run --rm gateway create-merchant --name "Demo Store" --webhook-url http://webhook-receiver:8081/webhooks
