# syntax=docker/dockerfile:1.7

# The boot artifact is built separately so changes to the guest filesystem do
# not force another kernel compilation.
FROM debian:bookworm AS kernel-builder
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
COPY oci-images/fastboot/kernel.config /src/kernel.config
WORKDIR /src/linux
RUN make x86_64_defconfig \
    && make kvm_guest.config \
    && scripts/kconfig/merge_config.sh -m .config /src/kernel.config \
    && make olddefconfig \
    && grep -qx 'CONFIG_PVH=y' .config \
    && grep -qx 'CONFIG_VIRTIO_BLK=y' .config \
    && grep -qx 'CONFIG_VIRTIO_NET=y' .config \
    && grep -qx 'CONFIG_VIRTIO_VSOCKETS=y' .config \
    && grep -qx 'CONFIG_EROFS_FS=y' .config \
    && grep -qx 'CONFIG_OVERLAY_FS=y' .config \
    && grep -qx 'CONFIG_EXT4_FS=y' .config \
    && grep -qx '# CONFIG_MODULES is not set' .config \
    && make -j"$(nproc)" vmlinux \
    && install -Dm 0644 vmlinux /out/boot/vmlinuz-fast

FROM ubuntu:24.04 AS initrd-builder
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
    busybox-static cpio && rm -rf /var/lib/apt/lists/*
COPY oci-images/fastboot/init /src/init
RUN set -eu; \
    install -d /rootfs/bin /rootfs/dev /rootfs/proc /rootfs/sys /out/boot; \
    install -m 0755 /bin/busybox /rootfs/bin/busybox; \
    install -m 0755 /src/init /rootfs/init; \
    for applet in sh cat mount mkdir sleep hostname rm ln switch_root; do \
      ln -s busybox "/rootfs/bin/$applet"; \
    done; \
    cd /rootfs; \
    find . -print0 | cpio --null -o -H newc --quiet > /out/boot/initrd.img-fast

FROM scratch
COPY --from=kernel-builder /out/boot/vmlinuz-fast /boot/vmlinuz-fast
COPY --from=initrd-builder /out/boot/initrd.img-fast /boot/initrd.img-fast
