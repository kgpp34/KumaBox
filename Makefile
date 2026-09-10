.PHONY: all build install test doctor-check race verify lint vet fmt fmt-check deps clean coverage cloc help

REPO_PATH := github.com/kumabox/kumabox

## Target OSes for vet / lint
GOOSES ?= linux darwin
REVISION := $(shell git rev-parse HEAD || echo unknown)
BUILTAT := $(shell date +%Y-%m-%dT%H:%M:%S)
VERSION := $(shell git describe --tags $(shell git rev-list --tags --max-count=1) 2>/dev/null || echo dev)
GO_LDFLAGS ?= -X $(REPO_PATH)/version.Commit=$(REVISION) \
              -X $(REPO_PATH)/version.BuildTime=$(BUILTAT) \
              -X $(REPO_PATH)/version.Version=$(VERSION)

ifneq ($(KEEP_SYMBOL), 1)
	GO_LDFLAGS += -s
endif

## Location to install dependencies and build outputs
LOCALBIN ?= $(shell pwd)/bin
PREFIX ?= /usr/local
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool versions
GOLANGCILINT_VERSION ?= v2.13.2
GOLANGCILINT_ROOT := $(LOCALBIN)/golangci-lint-$(GOLANGCILINT_VERSION)
GOLANGCILINT := $(GOLANGCILINT_ROOT)/golangci-lint

GOFUMPT_VERSION ?= v0.11.0
GOIMPORTS_VERSION ?= v0.49.0
GOFMT := $(LOCALBIN)/gofumpt-$(GOFUMPT_VERSION)
GOIMPORTS := $(LOCALBIN)/goimports-$(GOIMPORTS_VERSION)

## Tool download targets
.PHONY: golangci-lint
golangci-lint: $(GOLANGCILINT)
$(GOLANGCILINT):
	GOBIN=$(GOLANGCILINT_ROOT) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCILINT_VERSION)

.PHONY: gofumpt
gofumpt: $(GOFMT)
$(GOFMT): | $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install mvdan.cc/gofumpt@$(GOFUMPT_VERSION)
	mv $(LOCALBIN)/gofumpt $(GOFMT)

.PHONY: goimports
goimports: $(GOIMPORTS)
$(GOIMPORTS): | $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install golang.org/x/tools/cmd/goimports@$(GOIMPORTS_VERSION)
	mv $(LOCALBIN)/goimports $(GOIMPORTS)

# --- Primary targets ---

all: deps fmt lint test build ## Full pipeline: deps, fmt, lint, test, build

# --- Dependencies ---

deps: ## Tidy Go modules
	go mod tidy

# --- Build ---

build: | $(LOCALBIN) ## Build kumabox and kumabox-check
	CGO_ENABLED=0 go build -ldflags "$(GO_LDFLAGS)" -o $(LOCALBIN)/kumabox .
	cp doctor/check.sh $(LOCALBIN)/kumabox-check
	chmod 0755 $(LOCALBIN)/kumabox-check

install: build ## Install kumabox and kumabox-check
	install -d "$(DESTDIR)$(PREFIX)/bin"
	install -m 0755 $(LOCALBIN)/kumabox "$(DESTDIR)$(PREFIX)/bin/kumabox"
	install -m 0755 $(LOCALBIN)/kumabox-check "$(DESTDIR)$(PREFIX)/bin/kumabox-check"

# --- Testing ---

test: vet ## Run tests with race detection and coverage
	go test -race -timeout 120s -count=1 -cover -coverprofile=coverage.out ./...

doctor-check: ## Check the doctor script syntax
	bash -n doctor/check.sh

race: ## Run all Go tests with race detection
	go test -race ./...

verify: fmt-check vet doctor-check test build ## Verify formatting, tests and build

coverage: test ## Generate and display coverage report
	go tool cover -func=coverage.out
	@echo ""
	@echo "To view HTML coverage report: go tool cover -html=coverage.out"

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
	$(GOFMT) -extra -l -w .
	$(GOIMPORTS) -l -w --local 'github.com/kumabox/kumabox' .

fmt-check: gofumpt goimports ## Check formatting (fails if files need formatting)
	@test -z "$$($(GOFMT) -extra -l .)" || { echo "Files need formatting (gofumpt):"; $(GOFMT) -extra -l .; exit 1; }
	@test -z "$$($(GOIMPORTS) -l .)" || { echo "Files need formatting (goimports):"; $(GOIMPORTS) -l .; exit 1; }

# --- Maintenance ---

clean: ## Remove build artifacts, coverage files, and test cache
	rm -f kumabox kumabox-linux-* kumabox-darwin-*
	rm -rf bin/ dist/
	rm -f coverage.out coverage.html coverage.txt
	go clean -testcache

cloc: ## Count lines of code excluding tests (requires cloc)
	cloc --exclude-dir=vendor,dist --exclude-ext=json --not-match-f='_test\.go$$' .

# --- Help ---

help: ## Show this help message
	@echo "KumaBox Makefile targets:"
	@echo ""
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'
	@echo ""
