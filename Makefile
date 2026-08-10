# Relata development tasks.
#
# Run `make help` for a summary.

SHELL := /bin/bash
.DEFAULT_GOAL := help

BINARY      := relata
CMD_PKG     := ./cmd/relata
BUILD_DIR   := bin
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)
GO          ?= go
DOCKER      ?= docker
COMPOSE     ?= $(DOCKER) compose

# `make setup` installs the linters into the Go bin directory, which is not
# necessarily on PATH. Look there explicitly so the tools are found either way.
GOBIN_DIR   := $(shell $(GO) env GOBIN)
ifeq ($(GOBIN_DIR),)
GOBIN_DIR   := $(shell $(GO) env GOPATH)/bin
endif
export PATH := $(GOBIN_DIR):$(PATH)

# Integration tests need real MongoDB and Qdrant instances, so they sit behind
# a build tag and are not run by `make test`.
INTEGRATION_TAG := integration

.PHONY: help
help: ## Show this help
	@echo "Relata — development tasks"
	@echo
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
	@echo
	@echo "Typical first run:"
	@echo "  make setup && make compose-up && make models && make createadmin && make dev"

.PHONY: setup
setup: ## Install tooling and fetch dependencies
	@echo "==> Fetching Go dependencies"
	$(GO) mod download
	$(GO) mod verify
	@echo "==> Installing development tools"
	@$(GO) install golang.org/x/vuln/cmd/govulncheck@latest 2>/dev/null || \
		echo "    (govulncheck unavailable — 'make security' will skip it)"
	@$(GO) install honnef.co/go/tools/cmd/staticcheck@latest 2>/dev/null || \
		echo "    (staticcheck unavailable — 'make lint' will use go vet only)"
	@if [ ! -f .env ]; then cp .env.example .env && chmod 600 .env && echo "==> Created .env from .env.example (mode 600)"; fi
	@mkdir -p data/uploads $(BUILD_DIR)
	@echo "==> Setup complete"

.PHONY: dev
dev: ## Run Relata from source against the compose services
	@mkdir -p data/uploads
	$(GO) run $(CMD_PKG) serve

.PHONY: build
build: ## Build the binary into ./bin
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) $(CMD_PKG)
	@echo "==> Built $(BUILD_DIR)/$(BINARY) ($(VERSION))"

.PHONY: test
test: ## Run unit tests with the race detector
	$(GO) test -race -count=1 ./...

.PHONY: test-short
test-short: ## Run unit tests without the race detector (faster)
	$(GO) test -count=1 ./...

.PHONY: test-integration
test-integration: ## Run integration tests (needs Docker for MongoDB and Qdrant)
	@echo "==> Integration tests use disposable containers; this takes a few minutes."
	$(GO) test -race -count=1 -tags=$(INTEGRATION_TAG) -timeout=40m ./tests/...

# The security regression tests span both levels: the unit tests need nothing
# running, while the end-to-end ones need MongoDB and Qdrant and skip cleanly
# when those are absent.
SECURITY_PATTERN := 'Security|XSS|Injection|Traversal|Workspace|CSRF|Oversized|Malformed|Prompt|Rate|Enumerat'

.PHONY: test-security
test-security: ## Run the security regression tests (unit and end-to-end)
	@echo "==> Unit-level security tests"
	$(GO) test -race -count=1 -run $(SECURITY_PATTERN) ./internal/...
	@echo "==> End-to-end security tests (skipped if MongoDB and Qdrant are not running)"
	@# Every test in tests/security_test.go carries the TestSecurity_ prefix, so
	@# this selector picks up new ones without the Makefile needing an edit.
	$(GO) test -race -count=1 -tags=$(INTEGRATION_TAG) -timeout=15m -run 'TestSecurity_' ./tests/...

.PHONY: test-cover
test-cover: ## Run tests and report coverage
	$(GO) test -count=1 -coverprofile=coverage.out -covermode=atomic ./...
	$(GO) tool cover -func=coverage.out | tail -1
	@echo "==> Full report: go tool cover -html=coverage.out"

.PHONY: fuzz
fuzz: ## Run each fuzz target briefly (parsers, model output, API input, scoring)
	@# Patterns are anchored: -fuzz takes a regular expression, and an unanchored
	@# name that is a prefix of another target matches both and runs neither.
	$(GO) test ./internal/parsing/  -run=Fuzz -fuzz='^FuzzParseText$$'   -fuzztime=30s
	$(GO) test ./internal/parsing/  -run=Fuzz -fuzz='^FuzzParseJSON$$'   -fuzztime=30s
	$(GO) test ./internal/parsing/  -run=Fuzz -fuzz='^FuzzSniff$$'       -fuzztime=30s
	$(GO) test ./internal/analysis/ -run=Fuzz -fuzz='^FuzzDecodeStrict$$' -fuzztime=30s
	$(GO) test ./internal/analysis/ -run=Fuzz -fuzz='^FuzzFence$$'       -fuzztime=20s
	$(GO) test ./internal/entities/ -run=Fuzz -fuzz='^FuzzNormalise$$'   -fuzztime=20s
	$(GO) test ./internal/auth/     -run=Fuzz -fuzz='^FuzzDecodeHash$$'  -fuzztime=20s
	$(GO) test ./internal/api/      -run=Fuzz -fuzz='^FuzzDecodeJSON$$'  -fuzztime=20s
	$(GO) test ./internal/api/      -run=Fuzz -fuzz='^FuzzQueryFilters$$' -fuzztime=20s
	$(GO) test ./internal/search/   -run=Fuzz -fuzz='^FuzzExcerptHelpers$$' -fuzztime=20s
	$(GO) test ./internal/relationships/ -run=Fuzz -fuzz='^FuzzExcerpt$$' -fuzztime=20s
	$(GO) test ./internal/relationships/ -run=Fuzz -fuzz='^FuzzScore$$'   -fuzztime=20s
	$(GO) test ./internal/entities/ -run=Fuzz -fuzz='^FuzzExtractDeterministic$$' -fuzztime=20s
	$(GO) test ./internal/entities/ -run=Fuzz -fuzz='^FuzzExtractDeterministicClaims$$' -fuzztime=20s
	$(GO) test ./internal/ingest/   -run=Fuzz -fuzz='^FuzzSanitiseFilename$$' -fuzztime=20s

.PHONY: lint
lint: fmt-check ## Run go vet and staticcheck
	@echo "==> go vet"
	$(GO) vet ./...
	@if command -v staticcheck >/dev/null 2>&1; then \
		echo "==> staticcheck"; staticcheck ./...; \
	else \
		echo "==> staticcheck not installed; run 'make setup' to add it"; \
	fi

.PHONY: fmt
fmt: ## Format the code
	$(GO) fmt ./...
	@command -v gofmt >/dev/null && gofmt -s -w .

.PHONY: fmt-check
fmt-check: ## Fail if any file is not gofmt-clean
	@unformatted=$$(gofmt -s -l . 2>/dev/null | grep -v '^$$' || true); \
	if [ -n "$$unformatted" ]; then \
		echo "These files are not formatted (run 'make fmt'):"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: security
security: ## Run vulnerability and static security checks
	@echo "==> Dependency vulnerability scan"
	@if command -v govulncheck >/dev/null 2>&1; then \
		govulncheck ./...; \
	else \
		echo "    govulncheck not installed; run 'make setup'"; \
	fi
	@echo "==> Security regression tests"
	@$(MAKE) --no-print-directory test-security
	@echo "==> Checking that no secrets are committed"
	@if git ls-files --error-unmatch .env >/dev/null 2>&1; then \
		echo "    .env is tracked by git — it must not be committed"; exit 1; \
	else echo "    .env is not tracked"; fi

.PHONY: compose-up
compose-up: ## Start MongoDB, Qdrant and Ollama
	$(COMPOSE) up -d mongodb qdrant ollama
	@echo "==> Waiting for services to report healthy…"
	@for i in $$(seq 1 60); do \
		if $(COMPOSE) ps --format json 2>/dev/null | grep -q '"Health":"starting"'; then sleep 2; else break; fi; \
	done
	@$(COMPOSE) ps
	@echo
	@echo "Next: make models   (pulls the Ollama models — a few GB, once)"

.PHONY: compose-down
compose-down: ## Stop the services, keeping data
	$(COMPOSE) down

.PHONY: compose-logs
compose-logs: ## Follow the service logs
	$(COMPOSE) logs -f

.PHONY: models
models: ## Pull the Ollama models Relata uses
	$(COMPOSE) --profile setup up ollama-pull

.PHONY: createadmin
createadmin: ## Create the initial administrator account
	$(GO) run $(CMD_PKG) createadmin --username $${RELATA_ADMIN_USERNAME:-admin}

.PHONY: seed
seed: ## Load the demonstration dataset
	$(GO) run $(CMD_PKG) seed

.PHONY: seed-wait
seed-wait: ## Load the demonstration dataset and wait for processing
	$(GO) run $(CMD_PKG) seed --wait

.PHONY: docker-build
docker-build: ## Build the production container image
	$(DOCKER) build --build-arg VERSION=$(VERSION) -t relata:$(VERSION) -t relata:latest .

.PHONY: docker-run
docker-run: ## Run Relata in a container against the compose services
	$(COMPOSE) --profile app up -d --build app
	@echo "==> Relata is at http://127.0.0.1:8080"

.PHONY: openapi
openapi: ## Verify the OpenAPI specification against the implemented routes
	@echo "==> Checking docs/openapi.yaml against the registered routes"
	$(GO) test ./internal/api/ -run OpenAPI -count=1
	@echo "==> docs/openapi.yaml matches the implementation"

.PHONY: backup
backup: ## Back up MongoDB and the uploaded originals
	./scripts/backup.sh

.PHONY: reset
reset: ## Delete all local data and start again (destructive)
	@echo "This deletes every stored item, vector and account."
	@read -p "Type 'reset' to confirm: " ans && [ "$$ans" = "reset" ] || (echo "Cancelled."; exit 1)
	$(COMPOSE) down -v
	rm -rf data/uploads/*
	@echo "==> Local data removed. Run 'make compose-up && make createadmin' to start again."

.PHONY: clean
clean: ## Remove build artefacts
	rm -rf $(BUILD_DIR) coverage.out
	$(GO) clean -testcache
	@echo "==> Cleaned"

.PHONY: check
check: fmt-check lint test ## Everything CI runs on a pull request
	@echo "==> All checks passed"
