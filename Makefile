# Task runner for the service. `make help` lists the targets.
SHELL := /bin/bash
.DEFAULT_GOAL := help

BINARY     ?= msvcservice
# GOWORK=off: in a workspace (go work use ../go-msvc-kit) `go list -m` names every module in it.
MODULE     := $(shell GOWORK=off go list -m)
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS    := -s -w -X main.version=$(VERSION)
GOFLAGS    ?=
PKGS       := ./...
INTEGRATION_TAG := integration

# The platform kit is a private module on GitHub (README, "kit"). An image build receives the
# credential as a BuildKit secret: one netrc line in KIT_NETRC. The pipeline sets it from a secret
# variable; locally it is built from the GitHub CLI's token. Make echoes the recipe, not the token.
KIT_MODULE := github.com/guangliangus/go-msvc-kit
KIT_NETRC_LOCAL = machine github.com login x-access-token password $$(gh auth token)

## ---- build & run -----------------------------------------------------------

.PHONY: build
build: ## Build the binary into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

.PHONY: run
run: ## Run the API host locally: API on :8080, probes + /metrics on :8081 (compose infrastructure expected)
	# The development overlay (internal/config/development.yaml) already points the inventory client
	# at the compose WireMock (localhost:8089) — nothing else to set here.
	APP_ENV=development go run ./cmd/$(BINARY) serve

.PHONY: run-worker
run-worker: ## Run the worker host locally: probes + /metrics on :8082, no API listener
	APP_ENV=development SERVER__ADMIN_ADDR=:8082 go run ./cmd/$(BINARY) worker

## ---- quality ---------------------------------------------------------------

.PHONY: fmt
fmt: ## gofmt + goimports
	gofmt -s -w .
	@command -v goimports >/dev/null && goimports -local $(MODULE),$(KIT_MODULE) -w . || true

.PHONY: vet
vet: ## go vet
	go vet $(PKGS)

# The same pin as azure-pipelines.yml (golangciLintVersion; Renovate bumps both in one PR). A local
# binary of another major.minor reports different findings than the pipeline will, so `make lint`
# says so — and still runs, because a warning is not a reason to skip linting.
# renovate: datasource=github-releases depName=golangci/golangci-lint
GOLANGCI_LINT_VERSION := v2.14.0

.PHONY: lint
lint: ## golangci-lint (install: brew install golangci-lint; the pipeline's pin is GOLANGCI_LINT_VERSION)
	@want=$$(echo "$(GOLANGCI_LINT_VERSION)" | sed -E 's/^v//; s/^([0-9]+\.[0-9]+).*/\1/'); \
	have=$$(golangci-lint version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+(\.[0-9]+)?' | head -1 | sed -E 's/^([0-9]+\.[0-9]+).*/\1/'); \
	if [ "$$have" != "$$want" ]; then \
	  echo "warning: local golangci-lint $${have:-not found} is not the pipeline's $(GOLANGCI_LINT_VERSION) (major.minor $$want): findings may differ"; \
	fi
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
	go test -race -count=1 -tags=$(INTEGRATION_TAG) -timeout 15m ./tests/...

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
up-app: ## Start the infrastructure plus the service built from this tree (kit credential: KIT_NETRC or gh)
	KIT_NETRC="$${KIT_NETRC:-$(KIT_NETRC_LOCAL)}" docker compose --profile app --profile worker up -d --build
	docker compose wait schema

.PHONY: down
down: ## Stop the compose stack and delete its volumes
	docker compose --profile app --profile worker --profile kafka down -v

.PHONY: e2e
e2e: ## End-to-end acceptance against the compose stack
	tools/e2e.sh

## ---- packaging -------------------------------------------------------------

.PHONY: docker
docker: ## Build the container image (kit credential: KIT_NETRC or gh)
	KIT_NETRC="$${KIT_NETRC:-$(KIT_NETRC_LOCAL)}" docker build --secret id=netrc,env=KIT_NETRC \
		--build-arg VERSION=$(VERSION) -t $(BINARY):$(VERSION) -t $(BINARY):local .

.PHONY: helm-lint
helm-lint: ## helm lint + template: both overlays, plus the worker + KEDA shape (RabbitMQ and Kafka triggers)
	helm lint deploy/helm/$(BINARY)
	helm template $(BINARY) deploy/helm/$(BINARY) -f deploy/helm/$(BINARY)/values.dev.yaml > /dev/null
	helm template $(BINARY) deploy/helm/$(BINARY) -f deploy/helm/$(BINARY)/values.prod.yaml > /dev/null
	helm template $(BINARY) deploy/helm/$(BINARY) -f deploy/helm/$(BINARY)/values.prod.yaml --set worker.enabled=true \
		--set worker.keda.enabled=true --set worker.keda.amqpConnection=amqp://keda:keda@rabbitmq:5672/ > /dev/null
	helm template $(BINARY) deploy/helm/$(BINARY) -f deploy/helm/$(BINARY)/values.prod.yaml --set worker.enabled=true \
		--set worker.keda.enabled=true --set worker.keda.trigger=kafka --set worker.keda.kafka.bootstrapServers=kafka:9092 \
		--set worker.keda.kafka.sasl=scram_sha512 --set worker.keda.kafka.username=keda \
		--set worker.keda.kafka.password=keda > /dev/null

.PHONY: new-service
new-service: ## Generate a service from this template: make new-service NAME=orders MODULE=github.com/acme/orders
	tools/new-service.sh "$(NAME)" "$(MODULE)" $(DEST)

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
