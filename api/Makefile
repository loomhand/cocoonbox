.PHONY: all build test lint vet fmt fmt-check deps proto coverage clean help

BINARY_NAME := cocoonbox-api
GOIMPORTS_LOCAL_PREFIXES := github.com/CMGS/cocoonbox
PROTO_DIR := ../proto

## Target OSes for vet / lint
GOOSES ?= linux darwin

BUILD_OUT ?= bin/$(BINARY_NAME)
ifneq ($(GOOS),)
ifneq ($(GOARCH),)
BUILD_OUT := bin/$(BINARY_NAME)-$(GOOS)-$(GOARCH)
endif
endif

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool versions
GOLANGCILINT_VERSION ?= v2.13.2
GOLANGCILINT_ROOT := $(LOCALBIN)/golangci-lint-$(GOLANGCILINT_VERSION)
GOLANGCILINT := $(GOLANGCILINT_ROOT)/golangci-lint

GOFUMPT_VERSION ?= v0.11.0
GOFUMPT_ROOT := $(LOCALBIN)/gofumpt-$(GOFUMPT_VERSION)
GOFMT := $(GOFUMPT_ROOT)/gofumpt

GOIMPORTS_VERSION ?= v0.49.0
GOIMPORTS_ROOT := $(LOCALBIN)/goimports-$(GOIMPORTS_VERSION)
GOIMPORTS := $(GOIMPORTS_ROOT)/goimports

PROTOCGENGO_VERSION ?= v1.36.12
PROTOCGENGO_ROOT := $(LOCALBIN)/protoc-gen-go-$(PROTOCGENGO_VERSION)
PROTOCGENGO := $(PROTOCGENGO_ROOT)/protoc-gen-go

PROTOCGENGOGRPC_VERSION ?= v1.6.2
PROTOCGENGOGRPC_ROOT := $(LOCALBIN)/protoc-gen-go-grpc-$(PROTOCGENGOGRPC_VERSION)
PROTOCGENGOGRPC := $(PROTOCGENGOGRPC_ROOT)/protoc-gen-go-grpc

## Tool download targets
.PHONY: golangci-lint
golangci-lint: $(GOLANGCILINT)
$(GOLANGCILINT):
	GOBIN=$(GOLANGCILINT_ROOT) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCILINT_VERSION)

.PHONY: gofumpt
gofumpt: $(GOFMT)
$(GOFMT): | $(LOCALBIN)
	GOBIN=$(GOFUMPT_ROOT) go install mvdan.cc/gofumpt@$(GOFUMPT_VERSION)

.PHONY: goimports
goimports: $(GOIMPORTS)
$(GOIMPORTS): | $(LOCALBIN)
	GOBIN=$(GOIMPORTS_ROOT) go install golang.org/x/tools/cmd/goimports@$(GOIMPORTS_VERSION)

.PHONY: protoc-gen-go
protoc-gen-go: $(PROTOCGENGO)
$(PROTOCGENGO): | $(LOCALBIN)
	GOBIN=$(PROTOCGENGO_ROOT) go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOCGENGO_VERSION)

.PHONY: protoc-gen-go-grpc
protoc-gen-go-grpc: $(PROTOCGENGOGRPC)
$(PROTOCGENGOGRPC): | $(LOCALBIN)
	GOBIN=$(PROTOCGENGOGRPC_ROOT) go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOCGENGOGRPC_VERSION)

# --- Primary targets ---

all: deps fmt lint test build ## Full pipeline: deps, fmt, lint, test, build

# --- Dependencies ---

deps: ## Tidy Go modules
	go mod tidy

proto: protoc-gen-go protoc-gen-go-grpc ## Regenerate gen/ from ../proto (requires protoc)
	rm -rf gen
	protoc -I $(PROTO_DIR) \
		--plugin=protoc-gen-go=$(PROTOCGENGO) --plugin=protoc-gen-go-grpc=$(PROTOCGENGOGRPC) \
		--go_out=. --go_opt=module=github.com/CMGS/cocoonbox/api \
		--go-grpc_out=. --go-grpc_opt=module=github.com/CMGS/cocoonbox/api \
		$(shell cd $(PROTO_DIR) && find cocoonbox -name '*.proto')

# --- Build ---

build: ## Build cocoonbox-api binary
	CGO_ENABLED=0 go build -ldflags "-s" -o $(BUILD_OUT) ./cmd/cocoonbox-api

# --- Testing ---

test: vet ## Run tests with race detection and coverage
	go test -race -timeout 120s -count=1 -cover -coverprofile=coverage.out ./...

coverage: test ## Generate and display coverage report
	go tool cover -func=coverage.out

# --- Code quality ---

vet: ## Run go vet on every target OS
	@for goos in $(GOOSES); do \
		echo "==> go vet GOOS=$$goos"; \
		GOOS=$$goos go vet ./... || exit 1; \
	done

lint: golangci-lint ## Run golangci-lint on every target OS
	@for goos in $(GOOSES); do \
		echo "==> golangci-lint GOOS=$$goos"; \
		GOOS=$$goos $(GOLANGCILINT) run ./... || exit 1; \
	done

fmt: gofumpt goimports ## Format code with gofumpt and goimports
	$(GOFMT) -l -w $$(find . -name '*.go' -not -path './gen/*' -not -path './bin/*')
	$(GOIMPORTS) -l -w --local '$(GOIMPORTS_LOCAL_PREFIXES)' $$(find . -name '*.go' -not -path './gen/*' -not -path './bin/*')

fmt-check: gofumpt goimports ## Check formatting (fails if files need formatting)
	@test -z "$$($(GOFMT) -l $$(find . -name '*.go' -not -path './gen/*' -not -path './bin/*'))" || { echo "Files need formatting (gofumpt)"; exit 1; }
	@test -z "$$($(GOIMPORTS) -l --local '$(GOIMPORTS_LOCAL_PREFIXES)' $$(find . -name '*.go' -not -path './gen/*' -not -path './bin/*'))" || { echo "Files need formatting (goimports)"; exit 1; }

# --- Maintenance ---

clean: ## Remove build artifacts, coverage files, and test cache
	rm -rf bin/
	rm -f coverage.out
	go clean -testcache

# --- Help ---

help: ## Show this help message
	@echo "cocoonbox-api Makefile targets:"
	@echo ""
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'
	@echo ""
