FROM ubuntu:24.04

ARG TARGETARCH
ENV DEBIAN_FRONTEND=noninteractive

COPY overlay.sh /usr/local/share/kumabox/initramfs/kumabox-overlay
COPY network.sh /usr/local/share/kumabox/initramfs/kumabox-network
COPY agent-stub.sh /usr/local/bin/kumabox-agent
COPY kumabox-agent.service /etc/systemd/system/kumabox-agent.service

RUN apt-get update && apt-get install -y --no-install-recommends \
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
        tcpdump \
        udev \
    && install -m 0755 /usr/local/share/kumabox/initramfs/kumabox-overlay /etc/initramfs-tools/scripts/kumabox-overlay \
    && install -m 0755 /usr/local/share/kumabox/initramfs/kumabox-network /etc/initramfs-tools/scripts/init-bottom/kumabox-network \
    && chmod 0755 /usr/local/bin/kumabox-agent \
    && printf "erofs\noverlay\next4\nvirtio_blk\nvirtio_pci\nvirtio_ring\nvirtio_net\nvsock\nvhost_vsock\nvmw_vsock_virtio_transport\n" >> /etc/initramfs-tools/modules \
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
    && systemctl enable ssh \
    && rm -rf /var/lib/apt/lists/*

CMD ["/sbin/init"]
