.PHONY: build test lint e2e fixrate conformance fixtures-check rename-module help

# Build variables
# GO is overridable so a contributor whose toolchain is not on PATH can point
# at it (make GO=/opt/go/bin/go test) without editing this file. It must never
# be a hard-coded absolute path: this Makefile is what CI and every other
# contributor runs.
GO ?= go
BINARY_NAME=belay
BIN_DIR=./bin
VERSION?=dev
COMMIT?=$(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_DATE?=$(shell date -u +'%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || echo "unknown")
LD_FLAGS=-ldflags "-X github.com/belay-dev/belay/internal/cli.Version=$(VERSION) -X github.com/belay-dev/belay/internal/cli.Commit=$(COMMIT) -X github.com/belay-dev/belay/internal/cli.BuildDate=$(BUILD_DATE)"

# Default target
help:
	@echo "belay Makefile targets:"
	@echo "  build              - Build the belay binary to ./bin/belay"
	@echo "  test               - Run tests with race detector (default suite, includes conformance on unix)"
	@echo "  fixrate            - Run fix-rate harness (validates the fix-rate measurement tool)"
	@echo "  conformance        - Run conformance tests (unix only)"
	@echo "  e2e                - Run end-to-end tests (degrades gracefully if dir missing)"
	@echo "  fixtures-check     - Verify fixture modules (seeded-bug green, wc compiles)"
	@echo "  lint               - Run linting checks (go vet and golangci-lint)"
	@echo "  rename-module      - Rewrite module from belay-dev to OWNER (usage: make rename-module OWNER=<handle>)"
	@echo "  help               - Show this message"

build:
	@mkdir -p $(BIN_DIR)
	@echo "Building $(BINARY_NAME)..."
	$(GO) build $(LD_FLAGS) -o $(BIN_DIR)/$(BINARY_NAME) ./cmd/belay

test:
	@echo "Running tests with race detector..."
	$(GO) test ./... -race

fixrate:
	@echo "Running fix-rate harness..."
	$(GO) test ./test/fixrate/... -tags=fixrate -race

conformance:
	@if [ "$$(uname)" = "Darwin" ] || [ "$$(uname)" = "Linux" ]; then \
		echo "Running conformance tests (unix only)..."; \
		$(GO) test ./test/conformance/... -tags=unix -race; \
	else \
		echo "Conformance tests require unix; skipping on $$(uname)"; \
	fi

e2e:
	@if [ -d "./test/e2e" ]; then \
		echo "Running e2e tests..."; \
		$(GO) test ./test/e2e/... -tags=e2e -race; \
	else \
		echo "e2e test directory not found; skipping gracefully"; \
	fi

fixtures-check:
	@echo "Verifying fixture modules..."
	@echo "  Checking seeded-bug (must build and have green tests)..."
	@cd fixtures/seeded-bug && $(GO) test ./... -race && cd - > /dev/null
	@echo "  Checking wc (must build, tests may fail)..."
	@cd fixtures/wc && $(GO) build . && cd - > /dev/null
	@echo "OK: all fixtures verified"

lint:
	@echo "Running go vet..."
	$(GO) vet ./...
	@echo "Checking for golangci-lint..."
	@if command -v golangci-lint >/dev/null 2>&1; then \
		echo "Running golangci-lint..."; \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed; skipping (install via https://golangci-lint.run/usage/install/)"; \
	fi


rename-module:
	@if [ -z "$(OWNER)" ]; then \
		echo "Usage: make rename-module OWNER=<github_handle>"; \
		echo "Example: make rename-module OWNER=dhaam-ai"; \
		exit 1; \
	fi
	@OLD_MODULE=$$(grep '^module ' go.mod | awk '{print $$2}'); \
	echo "Renaming module from $$OLD_MODULE to github.com/$(OWNER)/belay..."; \
	$(GO) mod edit -module github.com/$(OWNER)/belay; \
	find . -name "*.go" -type f -exec sed -i '' "s|$$OLD_MODULE|github.com/$(OWNER)/belay|g" {} +; \
	echo "Verifying build after rename..."; \
	$(GO) build ./...; \
	echo "Module successfully renamed to github.com/$(OWNER)/belay"
