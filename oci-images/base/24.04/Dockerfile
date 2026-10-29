# syntax=docker/dockerfile:1.7

ARG UBUNTU_IMAGE=ubuntu:24.04
ARG BOOT_IMAGE=kumabox/boot:6.18.37
FROM ${BOOT_IMAGE} AS boot
FROM --platform=$BUILDPLATFORM golang:1.24.4-bookworm AS agent-builder

ARG TARGETARCH
ARG GOPROXY=https://proxy.golang.org,direct
ARG KUMABOX_VERSION=dev
ARG KUMABOX_COMMIT=unknown
ARG KUMABOX_BUILD_TIME=unknown
WORKDIR /src
COPY go.mod go.sum ./
COPY agent ./agent
COPY errdefs ./errdefs
COPY types ./types
COPY version ./version
COPY cmd/kumabox-agent ./cmd/kumabox-agent
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH="$TARGETARCH" GOPROXY="$GOPROXY" \
    go build -mod=readonly -trimpath \
    -ldflags="-s -w -X github.com/kumabox/kumabox/version.Version=$KUMABOX_VERSION -X github.com/kumabox/kumabox/version.Commit=$KUMABOX_COMMIT -X github.com/kumabox/kumabox/version.BuildTime=$KUMABOX_BUILD_TIME" \
    -o /out/kumabox-agent ./cmd/kumabox-agent

# Minimal glibc userland. Rust PID 1 owns boot and supervises the guest agent.
FROM ${UBUNTU_IMAGE}
ARG KUMABOX_VERSION=dev
ARG KUMABOX_COMMIT=unknown
ARG KUMABOX_BUILD_TIME=unknown
LABEL io.kumabox.boot.profile="overlay-v1" \
      io.kumabox.runtime.profile="native-v1" \
      org.opencontainers.image.version="$KUMABOX_VERSION" \
      org.opencontainers.image.revision="$KUMABOX_COMMIT" \
      org.opencontainers.image.created="$KUMABOX_BUILD_TIME" \
      org.opencontainers.image.source="https://github.com/kgpp34/KumaBox"
ENV DEBIAN_FRONTEND=noninteractive
COPY --from=boot /boot/ /boot/
COPY --from=agent-builder /out/kumabox-agent /usr/local/bin/kumabox-agent
COPY --from=boot /boot/kumabox-init /usr/local/sbin/kumabox-init
RUN set -eu; \
    apt-get -o Acquire::Retries=5 update; \
    apt-get -o Acquire::Retries=5 install -y --no-install-recommends \
      ca-certificates iproute2 iputils-ping curl; \
    install -d -m 0755 /var/lib/dbus /etc/kumabox /run /dev/pts /dev/shm; \
    truncate -s 0 /etc/fstab; \
    rm -f /etc/machine-id /var/lib/dbus/machine-id; \
    touch /etc/machine-id; \
    ln -s /etc/machine-id /var/lib/dbus/machine-id; \
    ln -s /usr/local/sbin/kumabox-init /sbin/init; \
    printf '%s\n' 'hosts: files dns' > /etc/nsswitch.conf; \
    ldconfig; \
    test -s /boot/vmlinuz-kumabox; test -s /boot/initrd.img-kumabox; \
    test -x /usr/local/bin/kumabox-agent; \
    test ! -e /usr/lib/systemd/systemd; \
    test -x /sbin/init; \
    { test ! -d /lib/modules || test -z "$(ls -A /lib/modules)"; }; \
    rm -rf /var/lib/apt/lists/*
CMD ["/sbin/init"]
