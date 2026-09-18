# syntax=docker/dockerfile:1.7

ARG UBUNTU_IMAGE=ubuntu:24.04
FROM --platform=$BUILDPLATFORM golang:1.24.4-bookworm AS agent-builder

ARG TARGETARCH
ARG GOPROXY=https://proxy.golang.org,direct
WORKDIR /src
COPY go.mod go.sum ./
COPY agent ./agent
COPY version ./version
COPY cmd/kumabox-agent ./cmd/kumabox-agent
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH="$TARGETARCH" GOPROXY="$GOPROXY" \
    go build -mod=readonly -trimpath -ldflags='-s -w' -o /out/kumabox-agent ./cmd/kumabox-agent

# Pin UBUNTU_IMAGE to a digest in release automation. Keeping it configurable
# also makes local architecture-specific acceptance builds straightforward.
FROM ${UBUNTU_IMAGE}

LABEL io.kumabox.boot.profile="overlay-v1"

ENV DEBIAN_FRONTEND=noninteractive

COPY --from=agent-builder /out/kumabox-agent /usr/local/bin/kumabox-agent
COPY oci-images/ubuntu/overlay.sh /usr/local/lib/kumabox/initramfs/kumabox-overlay
COPY oci-images/ubuntu/kumabox-agent.service /etc/systemd/system/kumabox-agent.service

RUN set -eu; \
    apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        initramfs-tools \
        kmod \
        linux-image-virtual \
        systemd \
        systemd-sysv \
        systemd-timesyncd \
        systemd-resolved \
        udev \
    && install -m 0755 /usr/local/lib/kumabox/initramfs/kumabox-overlay \
        /etc/initramfs-tools/scripts/kumabox-overlay \
    && printf '%s\n' \
        erofs overlay ext4 virtio_blk virtio_pci virtio_ring virtio_net \
        vsock vmw_vsock_virtio_transport \
        >> /etc/initramfs-tools/modules \
    && sed -i 's/^COMPRESS=.*/COMPRESS=gzip/' /etc/initramfs-tools/initramfs.conf \
    && sed -i '/^IP=/d' /etc/initramfs-tools/initramfs.conf \
    && printf 'IP=off\n' >> /etc/initramfs-tools/initramfs.conf \
    && update-initramfs -u -k all \
    && for module_dir in /lib/modules/*; do \
        kernel=${module_dir##*/}; \
        initrd=/boot/initrd.img-${kernel}; \
        contents=/tmp/initrd-${kernel}.list; \
        test -s /boot/vmlinuz-${kernel}; \
        test -s "$initrd"; \
        lsinitramfs "$initrd" > "$contents"; \
        grep -qx 'scripts/kumabox-overlay' "$contents"; \
        for module in erofs overlay ext4 virtio_blk virtio_pci virtio_net vsock vmw_vsock_virtio_transport; do \
            filename=$(modinfo -k "$kernel" -F filename "$module"); \
            if [ "$filename" != '(builtin)' ]; then \
                basename=${filename##*/}; \
                grep -q "/${basename}$" "$contents"; \
            fi; \
        done; \
        rm -f "$contents"; \
    done \
    && truncate -s 0 /etc/fstab \
    && systemctl mask systemd-fsck-root.service systemd-remount-fs.service systemd-fsck@.service \
    && systemctl enable systemd-networkd systemd-resolved systemd-timesyncd kumabox-agent.service \
    && install -d -m 0755 /etc/systemd/network \
    && printf '%s\n' \
        '[Match]' 'Name=en* eth*' '' '[Network]' 'DHCP=ipv4' '' \
        '[DHCPv4]' 'ClientIdentifier=mac' \
        > /etc/systemd/network/20-kumabox.network \
    && test -x /usr/local/bin/kumabox-agent \
    && test -L /etc/systemd/system/multi-user.target.wants/kumabox-agent.service \
    && rm -rf /var/lib/apt/lists/* /usr/local/lib/kumabox

CMD ["/sbin/init"]
