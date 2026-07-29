FROM ubuntu:24.04

ARG TARGETARCH=amd64
ARG APT_MIRROR=""
ARG APT_SECURITY_MIRROR=""
ENV DEBIAN_FRONTEND=noninteractive

COPY overlay.sh /usr/local/share/kumabox/initramfs/kumabox-overlay
COPY network.sh /usr/local/share/kumabox/initramfs/kumabox-network
COPY kumabox-agent-linux-amd64 /usr/local/share/kumabox/kumabox-agent-linux-amd64
COPY kumabox-agent-linux-arm64 /usr/local/share/kumabox/kumabox-agent-linux-arm64
COPY kumabox-agent.service /etc/systemd/system/kumabox-agent.service
COPY kumabox-agent.openrc /usr/local/share/kumabox/kumabox-agent.openrc

RUN if [ -n "$APT_MIRROR" ]; then \
        sed -i "s|http://archive.ubuntu.com/ubuntu|$APT_MIRROR|g; s|http://ports.ubuntu.com/ubuntu-ports|$APT_MIRROR|g" /etc/apt/sources.list /etc/apt/sources.list.d/*.sources 2>/dev/null || true; \
    fi \
    && if [ -n "$APT_SECURITY_MIRROR" ]; then \
        sed -i "s|http://security.ubuntu.com/ubuntu|$APT_SECURITY_MIRROR|g; s|http://ports.ubuntu.com/ubuntu-ports|$APT_SECURITY_MIRROR|g" /etc/apt/sources.list /etc/apt/sources.list.d/*.sources 2>/dev/null || true; \
    fi \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        initramfs-tools \
        iproute2 \
        iputils-ping \
        kmod \
        linux-image-virtual \
        openssh-server \
        systemd \
        systemd-resolved \
        systemd-sysv \
        systemd-timesyncd \
        udev \
        util-linux \
    && install -m 0755 /usr/local/share/kumabox/initramfs/kumabox-overlay /etc/initramfs-tools/scripts/kumabox-overlay \
    && install -m 0755 /usr/local/share/kumabox/initramfs/kumabox-network /etc/initramfs-tools/scripts/init-bottom/kumabox-network \
    && case "$TARGETARCH" in \
        amd64) install -m 0755 /usr/local/share/kumabox/kumabox-agent-linux-amd64 /usr/local/bin/kumabox-agent ;; \
        arm64) install -m 0755 /usr/local/share/kumabox/kumabox-agent-linux-arm64 /usr/local/bin/kumabox-agent ;; \
        *) echo "unsupported target architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac \
    && chmod 0755 /usr/local/share/kumabox/kumabox-agent.openrc \
    && printf "erofs\noverlay\next4\nvirtio_blk\nvirtio_pci\nvirtio_ring\nvirtio_net\nvsock\nvmw_vsock_virtio_transport\n" >> /etc/initramfs-tools/modules \
    && sed -i 's/^COMPRESS=.*/COMPRESS=gzip/' /etc/initramfs-tools/initramfs.conf \
    && sed -i '/^IP=/d' /etc/initramfs-tools/initramfs.conf \
    && echo 'IP=off' >> /etc/initramfs-tools/initramfs.conf \
    && update-initramfs -u -k all \
    && truncate -s 0 /etc/fstab \
    && systemctl mask systemd-fsck-root.service systemd-remount-fs.service systemd-fsck@.service \
    && systemctl enable systemd-networkd systemd-resolved systemd-timesyncd \
    && mkdir -p /etc/systemd/network /run/sshd \
    && printf "[Match]\nName=e* v*\n\n[Network]\nDHCP=yes\n\n[DHCPv4]\nClientIdentifier=mac\n" > /etc/systemd/network/20-wired.network \
    && sed -i 's/^#*PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config \
    && systemctl enable ssh kumabox-agent \
    && echo 'root:cocoon' | chpasswd \
    && rm -rf /var/lib/apt/lists/*

CMD ["/sbin/init"]
