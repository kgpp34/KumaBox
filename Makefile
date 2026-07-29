BINARY := kumabox
BIN_DIR := bin
GUEST_AGENT_BINARY_AMD64 := oci-images/ubuntu/kumabox-agent-linux-amd64
GUEST_AGENT_BINARY_ARM64 := oci-images/ubuntu/kumabox-agent-linux-arm64
VERSION ?= 0.0.0-dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -X github.com/kumabox/kumabox/internal/version.Version=$(VERSION) \
	-X github.com/kumabox/kumabox/internal/version.Commit=$(COMMIT) \
	-X github.com/kumabox/kumabox/internal/version.BuildTime=$(BUILD_TIME)

.PHONY: build build-agent build-agent-linux-amd64 build-agent-linux-arm64 test install-doctor clean

build: build-agent
	mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) ./cmd/kumabox

build-agent:
	$(MAKE) build-agent-linux-amd64 build-agent-linux-arm64

build-agent-linux-amd64:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o $(GUEST_AGENT_BINARY_AMD64) ./cmd/agent

build-agent-linux-arm64:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o $(GUEST_AGENT_BINARY_ARM64) ./cmd/agent

test:
	go test ./...

install-doctor:
	install -m 0755 scripts/linux/kumabox-doctor.sh /usr/local/bin/kumabox-doctor

clean:
	rm -rf $(BIN_DIR)
