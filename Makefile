GO ?= go
BIN_DIR ?= bin
PREFIX ?= /usr/local

# Packages in this module. Empty until the first phase lands code.
PKGS := $(shell $(GO) list ./... 2>/dev/null)

.PHONY: build install fmt-check test shell-test race vet verify

build:
	@mkdir -p "$(BIN_DIR)"
	$(GO) build -o "$(BIN_DIR)/kumabox" .
	@cp doctor/check.sh "$(BIN_DIR)/kumabox-check"
	@chmod 0755 "$(BIN_DIR)/kumabox-check"

install: build
	@install -d "$(DESTDIR)$(PREFIX)/bin"
	@install -m 0755 "$(BIN_DIR)/kumabox" "$(DESTDIR)$(PREFIX)/bin/kumabox"
	@install -m 0755 "$(BIN_DIR)/kumabox-check" "$(DESTDIR)$(PREFIX)/bin/kumabox-check"

fmt-check:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

test:
	$(GO) test ./...
	$(MAKE) shell-test

shell-test:
	bash doctor/check_test.sh

# race is required for any change touching concurrency, workers, streams or
# reconciliation (docs/ARCHITECTURE.md §9).
race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

verify: fmt-check vet test build
