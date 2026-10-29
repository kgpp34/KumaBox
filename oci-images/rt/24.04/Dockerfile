# syntax=docker/dockerfile:1.7

# Extend base for development; publish rt for fewer virtio block devices.
ARG BASE_IMAGE=kumabox/base:24.04
FROM ${BASE_IMAGE} AS root

FROM scratch
ARG KUMABOX_VERSION=dev
ARG KUMABOX_COMMIT=unknown
ARG KUMABOX_BUILD_TIME=unknown
LABEL io.kumabox.boot.profile="overlay-v1" \
      io.kumabox.runtime.profile="native-v1" \
      org.opencontainers.image.version="$KUMABOX_VERSION" \
      org.opencontainers.image.revision="$KUMABOX_COMMIT" \
      org.opencontainers.image.created="$KUMABOX_BUILD_TIME" \
      org.opencontainers.image.source="https://github.com/kgpp34/KumaBox"
COPY --from=root / /
CMD ["/sbin/init"]
