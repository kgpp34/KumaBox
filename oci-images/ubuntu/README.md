# KumaBox Ubuntu guest image

This image declares `io.kumabox.boot.profile=overlay-v1`. Its initramfs owns the
KumaBox host/guest boot contract:

- `boot=kumabox-overlay` selects the root provider;
- `kumabox.layers=kumabox-layerN,...,kumabox-layer0` lists EROFS lower layers
  from top to base;
- `kumabox.cow=kumabox-cow` identifies the ext4 upper/work disk;
- block devices are resolved by virtio serial, never by `/dev/vdX` order.

Build a local architecture image with BuildKit:

```sh
docker buildx build --load --platform linux/amd64 \
  -t kumabox/ubuntu:24.04 oci-images/ubuntu
```

Release builds must set `UBUNTU_IMAGE` to an immutable Ubuntu manifest digest:

```sh
docker buildx build --platform linux/amd64,linux/arm64 \
  --build-arg UBUNTU_IMAGE=ubuntu@sha256:<manifest-digest> \
  -t ghcr.io/kgpp34/kumabox/ubuntu:24.04 --push oci-images/ubuntu
```

The Dockerfile fails its build unless the initrd contains the overlay provider
and every required filesystem, virtio, and vsock capability is either built
into the kernel or present in the generated initrd.
