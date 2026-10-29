# syntax=docker/dockerfile:1.7

# Boot artifacts are independent from the Ubuntu userland and agent builds.
FROM debian:bookworm AS kernel-builder
ARG TARGETARCH
ARG KERNEL_VERSION=6.18.37
ARG KERNEL_SHA256=a83cd200e6646db52866b8309e9137b9e9048b613cbda10ced2b811aae125255
RUN apt-get update && apt-get install -y --no-install-recommends \
    bc bison build-essential ca-certificates curl flex libelf-dev libssl-dev \
    python3 xz-utils && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN curl -fL --retry 5 -o linux.tar.xz \
      "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${KERNEL_VERSION}.tar.xz" \
    && echo "${KERNEL_SHA256}  linux.tar.xz" | sha256sum -c - \
    && mkdir linux && tar -xJf linux.tar.xz -C linux --strip-components=1 \
    && rm linux.tar.xz
COPY oci-images/boot/kernel*.config /src/
WORKDIR /src/linux
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) make x86_64_defconfig; make kvm_guest.config; \
             scripts/kconfig/merge_config.sh -m .config /src/kernel.config; \
             entry=PVH ;; \
      arm64) make defconfig; \
             scripts/kconfig/merge_config.sh -m .config /src/kernel.config /src/kernel-arm64.config; \
             entry=RTC_DRV_PL031 ;; \
      *) echo "unsupported guest architecture: $TARGETARCH"; exit 1 ;; \
    esac; \
    make olddefconfig; \
    for symbol in "$entry" PROC_FS SYSFS DEVTMPFS BLK_DEV_INITRD VIRTIO_PCI VIRTIO_BLK \
        VIRTIO_NET VIRTIO_CONSOLE VIRTIO_VSOCKETS EROFS_FS EROFS_FS_ZIP \
        EXT4_FS OVERLAY_FS OVERLAY_FS_INDEX OVERLAY_FS_METACOPY UNIX98_PTYS \
        USER_NS SECCOMP_FILTER CGROUPS MEMCG CPUSETS BRIDGE VETH \
        BRIDGE_NETFILTER NF_NAT NFT_COMPAT NETFILTER_XT_TARGET_MASQUERADE; do \
      grep -qx "CONFIG_${symbol}=y" .config || { echo "missing kernel capability: $symbol"; exit 1; }; \
    done; \
    grep -qx '# CONFIG_MODULES is not set' .config; \
    if [ "$TARGETARCH" = amd64 ]; then \
      make -j"$(nproc)" vmlinux; install -Dm 0644 vmlinux /out/boot/vmlinuz-kumabox; \
    else \
      make -j"$(nproc)" Image; install -Dm 0644 arch/arm64/boot/Image /out/boot/vmlinuz-kumabox; \
    fi; \
    install -Dm 0644 .config /out/boot/config-kumabox; \
    cc -O2 usr/gen_init_cpio.c -o /out/gen_init_cpio

FROM rust:1.89-alpine AS init-builder
RUN apk add --no-cache musl-dev binutils
WORKDIR /src
COPY oci-images/boot/init/Cargo.toml oci-images/boot/init/Cargo.lock ./
COPY oci-images/boot/init/src ./src
RUN cargo build --release --locked \
    && readelf -l target/release/kumabox-init > /tmp/program-headers \
    && ! grep -q INTERP /tmp/program-headers

FROM busybox:musl AS debug-tools

FROM kernel-builder AS initrd-builder
ARG INITRD_DEBUG=0
WORKDIR /archive
COPY --from=init-builder /src/target/release/kumabox-init /archive/kumabox-init
COPY --from=debug-tools /bin/busybox /archive/busybox
# Explicit metadata makes /proc and /sys unconditional and needs no privileged
# mknod. The small newc archive is deliberately uncompressed.
RUN set -eu; \
    printf '%s\n' \
      'dir /dev 0755 0 0' 'nod /dev/console 0600 0 0 c 5 1' \
      'dir /proc 0755 0 0' 'dir /sys 0755 0 0' 'dir /newroot 0755 0 0' \
      'dir /layers 0755 0 0' 'dir /cow 0755 0 0' \
      'file /init /archive/kumabox-init 0755 0 0' > initrd.list; \
    case "$INITRD_DEBUG" in \
      0) ;; \
      1) printf '%s\n' 'dir /bin 0755 0 0' \
           'file /bin/busybox /archive/busybox 0755 0 0' \
           'slink /bin/sh /bin/busybox 0777 0 0' >> initrd.list ;; \
      *) echo 'INITRD_DEBUG must be 0 or 1'; exit 1 ;; \
    esac; \
    /out/gen_init_cpio initrd.list > /out/boot/initrd.img-kumabox

FROM scratch
COPY --from=kernel-builder /out/boot/vmlinuz-kumabox /boot/vmlinuz-kumabox
COPY --from=kernel-builder /out/boot/config-kumabox /boot/config-kumabox
COPY --from=initrd-builder /out/boot/initrd.img-kumabox /boot/initrd.img-kumabox
CMD ["/boot"]
