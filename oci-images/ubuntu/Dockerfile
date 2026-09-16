# syntax=docker/dockerfile:1.7

# Pin UBUNTU_IMAGE to a digest in release automation. Keeping it configurable
# also makes local architecture-specific acceptance builds straightforward.
ARG UBUNTU_IMAGE=ubuntu:24.04
FROM ${UBUNTU_IMAGE}

LABEL io.kumabox.boot.profile="overlay-v1"

ENV DEBIAN_FRONTEND=noninteractive

COPY overlay.sh /usr/local/lib/kumabox/initramfs/kumabox-overlay

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
    && systemctl enable systemd-networkd systemd-resolved systemd-timesyncd \
    && install -d -m 0755 /etc/systemd/network \
    && printf '%s\n' \
        '[Match]' 'Name=en* eth*' '' '[Network]' 'DHCP=ipv4' '' \
        '[DHCPv4]' 'ClientIdentifier=mac' \
        > /etc/systemd/network/20-kumabox.network \
    && rm -rf /var/lib/apt/lists/* /usr/local/lib/kumabox

CMD ["/sbin/init"]
