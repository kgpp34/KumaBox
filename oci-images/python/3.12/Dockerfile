# syntax=docker/dockerfile:1.7
ARG RUNTIME_IMAGE=kumabox/rt:24.04
FROM ${RUNTIME_IMAGE} AS root
RUN apt-get -o Acquire::Retries=5 update \
    && apt-get -o Acquire::Retries=5 install -y --no-install-recommends python3.12 python3-venv python3-pip \
    && ln -s /usr/bin/python3 /usr/local/bin/python \
    && rm -rf /var/lib/apt/lists/*

# Flatten the final workload too: installing a language must not add root disks.
FROM scratch
LABEL io.kumabox.boot.profile="overlay-v1" io.kumabox.runtime.profile="native-v1" \
      io.kumabox.workload="python:3.12"
COPY --from=root / /
CMD ["/sbin/init"]
