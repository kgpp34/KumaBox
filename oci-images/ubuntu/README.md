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
  -f oci-images/ubuntu/Dockerfile \
  -t kumabox/ubuntu:24.04 .
```

Use `--build-arg GOPROXY=<proxy>,direct` when the default Go module proxy is
not reachable from the BuildKit worker.

Release builds must set `UBUNTU_IMAGE` to an immutable Ubuntu manifest digest:

```sh
docker buildx build --platform linux/amd64,linux/arm64 \
  --build-arg UBUNTU_IMAGE=ubuntu@sha256:<manifest-digest> \
  -f oci-images/ubuntu/Dockerfile \
  -t ghcr.io/kgpp34/kumabox/ubuntu:24.04 --push .
```

The Dockerfile fails its build unless the initrd contains the overlay provider
and every required filesystem, virtio, and vsock capability is either built
into the kernel or present in the generated initrd.

The same build compiles `kumabox-agent` from the checked-out source, installs
it in the guest, and enables `kumabox-agent.service`. No prebuilt agent binary
is required in the build context.
