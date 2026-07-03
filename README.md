# KumaBox

KumaBox is a microVM sandbox runtime for running agents, automation, and
untrusted workloads with stronger isolation than a normal container workflow.

It aims to keep the developer experience close to Docker while using hardware
virtualization boundaries underneath. Each sandbox runs inside its own microVM,
with local state, logs, images, and runtime metadata managed by the KumaBox CLI.

## Why KumaBox

Agents and automation often need to execute code, install packages, inspect
files, browse the network, or run tools supplied by a user. Containers are fast
and convenient, but they share a kernel with the host. KumaBox is designed for
workloads where that boundary is not strong enough.

KumaBox focuses on:

- Stronger isolation through microVMs.
- A simple CLI-first workflow without a required global daemon.
- Reproducible sandbox state stored on disk.
- Managed cloud images as the base for VM workloads.
- Clear lifecycle commands for create, run, inspect, logs, stop, delete, and
  cleanup.
- A path toward OCI image support, copy-on-write disks, networking, snapshots,
  and multiple VM backends.

## What It Does

KumaBox manages the local lifecycle of sandbox VMs:

```bash
kumabox doctor
kumabox image import ubuntu.img --name ubuntu --firmware CLOUDHV.fd
kumabox run ubuntu --name devbox
kumabox ps
kumabox inspect devbox --json
kumabox logs devbox
kumabox stop devbox
kumabox delete devbox
```

The runtime records VM metadata, backend configuration, process identity, logs,
and image metadata so that state can be inspected and reconciled even when a VM
or CLI process exits unexpectedly.

## Core Concepts

**Images**

KumaBox imports local cloud images and stores their metadata, source details,
disk format, size information, and checksums. Images are the reusable base for
creating sandbox VMs.

**VMs**

A VM record describes one sandbox instance: name, backend, disk, runtime
directories, logs, backend config, PID, API socket, and timestamps.

**Daemonless Runtime**

KumaBox does not require a long-running control daemon for basic local
operation. The CLI reads persisted state, checks the actual backend process, and
reports the observed VM state.

**Backends**

Cloud Hypervisor is the current backend. KumaBox keeps the backend boundary
explicit so future VM backends can be added without hiding capability
differences.

## Build

```bash
make build
```

The binary is written to `bin/kumabox`.

## Test

```bash
go test ./...
go vet ./...
```

## Status

KumaBox is under active development. The current implementation is suitable for
local experimentation with Cloud Hypervisor-backed microVM lifecycle and managed
cloud image metadata. Networking, OCI image builds, snapshots, and broader
backend support are planned capabilities.
