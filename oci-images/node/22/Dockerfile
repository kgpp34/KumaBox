# syntax=docker/dockerfile:1.7
ARG NODE_IMAGE=node:22-bookworm-slim
ARG RUNTIME_IMAGE=kumabox/rt:24.04
FROM ${NODE_IMAGE} AS node
FROM ${RUNTIME_IMAGE} AS root
COPY --from=node /usr/local/ /usr/local/
RUN apt-get -o Acquire::Retries=5 update \
    && apt-get -o Acquire::Retries=5 install -y --no-install-recommends libstdc++6 \
    && node --version && npm --version \
    && rm -rf /var/lib/apt/lists/*

FROM scratch
LABEL io.kumabox.boot.profile="overlay-v1" io.kumabox.runtime.profile="native-v1" \
      io.kumabox.workload="node:22"
COPY --from=root / /
CMD ["/sbin/init"]
