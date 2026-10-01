SHELL := /bin/bash
BASE_URL ?= http://localhost:8080

.PHONY: help up down reset logs run build test lint fmt burst

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-8s %s\n", $$1, $$2}'

up: ## Start app + Postgres with docker compose
	docker compose up --build -d
	@echo "waiting for app..."; for i in $$(seq 1 30); do curl -fs $(BASE_URL)/healthz >/dev/null && break; sleep 1; done
	@curl -fsS $(BASE_URL)/healthz && echo

down: ## Stop containers (keeps data)
	docker compose down

reset: ## Stop containers and delete the database volume
	docker compose down -v

logs: ## Follow app logs
	docker compose logs -f app

run: ## Run the server on the host using .env
	@test -f .env || (echo ".env missing: cp .env.example .env" && exit 1)
	set -a && source .env && set +a && go run ./cmd/server

build: ## Build the server binary into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/server ./cmd/server

TEST_DATABASE_URL ?= postgres://seats:seats@127.0.0.1:$${DB_PORT:-5432}/seats?sslmode=disable

test: ## Run all tests with the race detector (DB tests need `make up` first)
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test -race -count=1 ./...

lint: ## go vet + gofmt check
	go vet ./...
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

fmt: ## Format code
	gofmt -w .

burst: ## Burst + correctness checks against BASE_URL; exits non-zero on any failure or 5xx
	go run ./cmd/burst -base-url $(BASE_URL) $(BURST_FLAGS)
