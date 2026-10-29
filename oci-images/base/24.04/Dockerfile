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

# The base is already a fast-boot guest. rt only flattens its filesystem.
FROM ${UBUNTU_IMAGE}
ARG KUMABOX_VERSION=dev
ARG KUMABOX_COMMIT=unknown
ARG KUMABOX_BUILD_TIME=unknown
LABEL io.kumabox.boot.profile="overlay-v1" \
      org.opencontainers.image.version="$KUMABOX_VERSION" \
      org.opencontainers.image.revision="$KUMABOX_COMMIT" \
      org.opencontainers.image.created="$KUMABOX_BUILD_TIME" \
      org.opencontainers.image.source="https://github.com/kgpp34/KumaBox"
ENV DEBIAN_FRONTEND=noninteractive
COPY --from=boot /boot/ /boot/
COPY --from=agent-builder /out/kumabox-agent /usr/local/bin/kumabox-agent
COPY oci-images/base/24.04/kumabox-agent.service /etc/systemd/system/kumabox-agent.service
RUN set -eu; \
    apt-get -o Acquire::Retries=5 update; \
    apt-get -o Acquire::Retries=5 install -y --no-install-recommends \
      ca-certificates iproute2 iputils-ping systemd systemd-sysv \
      systemd-timesyncd systemd-resolved curl udev; \
    truncate -s 0 /etc/fstab; \
    systemctl mask systemd-fsck-root.service systemd-remount-fs.service \
      systemd-fsck@.service systemd-networkd-wait-online.service; \
    systemctl enable systemd-networkd systemd-resolved systemd-timesyncd kumabox-agent.service; \
    install -d -m 0755 /etc/systemd/network /var/lib/dbus; \
    printf '%s\n' '[Match]' 'Name=en* eth*' '' '[Network]' 'DHCP=ipv4' '' \
      '[DHCPv4]' 'ClientIdentifier=mac' > /etc/systemd/network/20-kumabox.network; \
    systemd-sysusers; \
    ldconfig; \
    journalctl --update-catalog; \
    /usr/lib/systemd/systemd-update-done; \
    rm -f /etc/machine-id /var/lib/dbus/machine-id /var/lib/systemd/random-seed; \
    touch /etc/machine-id; \
    ln -s /etc/machine-id /var/lib/dbus/machine-id; \
    test -s /boot/vmlinuz-kumabox; test -s /boot/initrd.img-kumabox; \
    test -x /usr/local/bin/kumabox-agent; \
    test -L /etc/systemd/system/sysinit.target.wants/kumabox-agent.service; \
    { test ! -d /lib/modules || test -z "$(ls -A /lib/modules)"; }; \
    rm -rf /var/lib/apt/lists/*
CMD ["/sbin/init"]
