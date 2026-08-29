.PHONY: build test lint e2e rename-module help

# Build variables
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
	@echo "  test               - Run tests with race detector"
	@echo "  lint               - Run linting checks (go vet and golangci-lint)"
	@echo "  e2e                - Run end-to-end tests (degrades gracefully if dir missing)"
	@echo "  rename-module      - Rewrite module from belay-dev to OWNER (usage: make rename-module OWNER=<handle>)"
	@echo "  help               - Show this message"

build:
	@mkdir -p $(BIN_DIR)
	@echo "Building $(BINARY_NAME)..."
	/Users/shashanksharma/.gobrew/current/bin/go build $(LD_FLAGS) -o $(BIN_DIR)/$(BINARY_NAME) ./cmd/belay

test:
	@echo "Running tests with race detector..."
	/Users/shashanksharma/.gobrew/current/bin/go test ./... -race

lint:
	@echo "Running go vet..."
	/Users/shashanksharma/.gobrew/current/bin/go vet ./...
	@echo "Checking for golangci-lint..."
	@if command -v golangci-lint >/dev/null 2>&1; then \
		echo "Running golangci-lint..."; \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed; skipping (install via https://golangci-lint.run/usage/install/)"; \
	fi

e2e:
	@if [ -d "./test/e2e" ]; then \
		echo "Running e2e tests..."; \
		/Users/shashanksharma/.gobrew/current/bin/go test ./test/e2e/... -tags=e2e; \
	else \
		echo "e2e test directory not found; skipping gracefully"; \
	fi

rename-module:
	@if [ -z "$(OWNER)" ]; then \
		echo "Usage: make rename-module OWNER=<github_handle>"; \
		echo "Example: make rename-module OWNER=dhaam-ai"; \
		exit 1; \
	fi
	@OLD_MODULE=$$(grep '^module ' go.mod | awk '{print $$2}'); \
	echo "Renaming module from $$OLD_MODULE to github.com/$(OWNER)/belay..."; \
	/Users/shashanksharma/.gobrew/current/bin/go mod edit -module github.com/$(OWNER)/belay; \
	find . -name "*.go" -type f -exec sed -i '' "s|$$OLD_MODULE|github.com/$(OWNER)/belay|g" {} +; \
	echo "Verifying build after rename..."; \
	/Users/shashanksharma/.gobrew/current/bin/go build ./...; \
	echo "Module successfully renamed to github.com/$(OWNER)/belay"
