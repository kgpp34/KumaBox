GO ?= go

.PHONY: build fmt-check test vet verify

build:
	$(GO) build ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

verify: fmt-check vet test build
