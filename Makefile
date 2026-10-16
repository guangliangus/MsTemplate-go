# Task runner for the service. `make help` lists the targets.
SHELL := /bin/bash
.DEFAULT_GOAL := help

BINARY     ?= msvcservice
MODULE     := $(shell go list -m)
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS    := -s -w -X main.version=$(VERSION)
GOFLAGS    ?=
PKGS       := ./...
INTEGRATION_TAG := integration

## ---- build & run -----------------------------------------------------------

.PHONY: build
build: ## Build the binary into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

.PHONY: run
run: ## Run the API host locally: API on :8080, probes + /metrics on :8081 (compose infrastructure expected)
	APP_ENV=development SERVICE_CLIENTS__INVENTORY__BASE_URL=http://localhost:8089/ go run ./cmd/$(BINARY) serve

.PHONY: run-worker
run-worker: ## Run the worker host locally: probes + /metrics on :8082, no API listener
	APP_ENV=development SERVER__ADMIN_ADDR=:8082 go run ./cmd/$(BINARY) worker

## ---- quality ---------------------------------------------------------------

.PHONY: fmt
fmt: ## gofmt + goimports
	gofmt -s -w .
	@command -v goimports >/dev/null && goimports -local $(MODULE) -w . || true

.PHONY: vet
vet: ## go vet
	go vet $(PKGS)

.PHONY: lint
lint: ## golangci-lint (install: brew install golangci-lint)
	golangci-lint run $(PKGS)

# CI tools are pinned in their own module (tools/go.mod, `tool` directives): one version for the
# laptop and the pipeline, checksum-verified by tools/go.sum, and never mixed into the service's
# own dependency graph.
TOOL := go tool -modfile=tools/go.mod

.PHONY: vuln
vuln: ## govulncheck (pinned in tools/go.mod) against the Go vulnerability database
	$(TOOL) govulncheck $(PKGS)

## ---- tests -----------------------------------------------------------------

.PHONY: test
test: ## Unit tests (plus the architecture and DDL guards)
	go test -race -count=1 $(PKGS)

.PHONY: test-integration
test-integration: ## Integration tests with Testcontainers (Docker required; CI=true makes a skip a failure)
	go test -race -count=1 -tags=$(INTEGRATION_TAG) -timeout 15m ./tests/... ./pkg/...

.PHONY: test-all
test-all: test test-integration ## Everything

.PHONY: cover
cover: ## Unit coverage report
	go test -count=1 -coverprofile=coverage.txt -covermode=atomic $(PKGS)
	go tool cover -html=coverage.txt -o coverage.html

## ---- local stack -----------------------------------------------------------

.PHONY: up
up: ## Start the compose infrastructure (schema applied by the one-shot service)
	docker compose up -d
	docker compose wait schema

.PHONY: up-app
up-app: ## Start the infrastructure plus the service built from this tree
	docker compose --profile app --profile worker up -d --build
	docker compose wait schema

.PHONY: down
down: ## Stop the compose stack and delete its volumes
	docker compose --profile app --profile worker down -v

.PHONY: e2e
e2e: ## End-to-end acceptance against the compose stack
	tools/e2e.sh

## ---- packaging -------------------------------------------------------------

.PHONY: docker
docker: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) -t $(BINARY):$(VERSION) -t $(BINARY):local .

.PHONY: helm-lint
helm-lint: ## helm lint + template both overlays
	helm lint deploy/helm/$(BINARY)
	helm template $(BINARY) deploy/helm/$(BINARY) -f deploy/helm/$(BINARY)/values.dev.yaml > /dev/null
	helm template $(BINARY) deploy/helm/$(BINARY) -f deploy/helm/$(BINARY)/values.prod.yaml > /dev/null

.PHONY: new-service
new-service: ## Generate a service from this template: make new-service NAME=orders MODULE=github.com/acme/orders
	tools/new-service.sh "$(NAME)" "$(MODULE)" $(DEST)

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
