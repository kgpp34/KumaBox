BINARY := kumabox
BIN_DIR := bin
VERSION ?= 0.0.0-dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -X github.com/kumabox/kumabox/internal/version.Version=$(VERSION) \
	-X github.com/kumabox/kumabox/internal/version.Commit=$(COMMIT) \
	-X github.com/kumabox/kumabox/internal/version.BuildTime=$(BUILD_TIME)

.PHONY: build test install-doctor clean

build:
	mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) ./cmd/kumabox

test:
	go test ./...

install-doctor:
	install -m 0755 scripts/linux/kumabox-doctor.sh /usr/local/bin/kumabox-doctor

clean:
	rm -rf $(BIN_DIR)
