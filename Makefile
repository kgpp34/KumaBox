GO ?= go

# Packages in this module. Empty until the first phase lands code.
PKGS := $(shell $(GO) list ./... 2>/dev/null)

.PHONY: build fmt-check test race vet verify

build:
	@if [ -n "$(PKGS)" ]; then $(GO) build ./...; else echo "no packages yet"; fi

fmt-check:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

test:
	@if [ -n "$(PKGS)" ]; then $(GO) test ./...; else echo "no packages yet"; fi

# race is required for any change touching concurrency, workers, streams or
# reconciliation (docs/ARCHITECTURE.md §9).
race:
	@if [ -n "$(PKGS)" ]; then $(GO) test -race ./...; else echo "no packages yet"; fi

vet:
	@if [ -n "$(PKGS)" ]; then $(GO) vet ./...; else echo "no packages yet"; fi

verify: fmt-check vet test build
