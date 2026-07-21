# P6: High-Density Performance

## 1. Phase Goal

P6 turns KumaBox from a functionally complete microVM runtime into a fast sandbox substrate for high-density agents. Performance is a product contract, not a later tuning exercise.

The default production path is fixed to:

```text
OCI image
  -> content-addressed EROFS layers
  -> sparse ext4 COW
  -> Cloud Hypervisor direct boot
  -> CNI netns + TAP + tc redirect
  -> guest agent over vsock
```

Cloud images, host-tap networking, UEFI boot and strict portable snapshot workflows remain supported compatibility paths, but they do not define the default latency target. Firecracker is deferred to P9; P6 must first make the Cloud Hypervisor path measurable and efficient.

P6 optimizes three user-visible operations:

1. Start a new sandbox and complete the first guest command.
2. Capture a reusable running snapshot with the shortest possible pause.
3. Restore or clone a sandbox and complete the first guest command.

### 1.1 Capability parity contract

P6 does not invent another snapshot or network model. Its hot path must match the proven Cloud Hypervisor behavior of the reference runtime:

| Capability | P6 default contract |
| --- | --- |
| Image and boot | OCI image, direct kernel boot, shared read-only EROFS layers and per-VM COW |
| Network | CNI allocation, per-VM netns, TAP and tc redirect |
| Running snapshot | Cloud Hypervisor native VM state, memory and writable disks |
| Restore modes | `copy`, `ondemand` and `mmap`, selected by measured workload trade-offs |
| Clone | Preserve workload state while allocating new storage, vsock CID, MAC, IP and CNI resources |
| Readiness | The restored agent accepts and completes the first command |

The default snapshot is the VMM-native crash-consistent snapshot. Guest filesystem freeze/thaw is not part of the parity target or the P6 performance path. Portable export remains an explicit offline operation and cannot add work to local snapshot or restore.

### 1.2 Full subsystem alignment

P6 uses the reference runtime's Cloud Hypervisor path as the capability floor. A difference is allowed only when a benchmark proves it faster without weakening isolation, or when the host/VMM cannot support the capability.

| Area | Behavior to align | P6 performance requirement |
| --- | --- | --- |
| Compute | Direct kernel boot, configurable vCPU/memory, hardware watchdog | No firmware or cloud-init on the OCI path; render only enabled devices |
| Guest readiness | Vsock agent as the machine control plane | Agent starts before general OS services; readiness means first exec completed |
| Memory | Hugepage capability detection, balloon with deflate-on-OOM and free-page reporting | Measure hugepage hit/miss cost; reclaim idle memory without hurting restore p95 |
| Restore memory | `copy`, `ondemand` userfaultfd and `mmap` CoW/page-cache sharing | Detect backend support instead of silently degrading; pin non-copy snapshot sources for VM lifetime |
| Root storage | Shared read-only OCI EROFS layers plus per-VM writable COW | No rootfs extraction or full copy at VM start |
| Block I/O | Virtio-blk multiqueue; page cache for read-only bases; O_DIRECT for writable COW/data disks when supported | Benchmark queue count/depth and fallback; avoid duplicate host+guest cache pressure |
| Sparse data | Sparse creation, reflink-first cloning, extent-aware fallback | Logical disk size must not become startup or snapshot copy cost |
| Network | CNI by default, per-VM netns, multiqueue TAP, bidirectional tc redirect | CNI ADD to usable guest address is included in ready latency |
| Network device | `num_queues = 2 * vCPU`, configurable queue size, VNET_HDR/GSO/checksum offload | Start with the aligned values, then change defaults only with end-to-end evidence |
| Network topology | MAC passthrough, TAP/veth MTU synchronization, multi-NIC and selectable conflist | No fixed bridge in the TAP-to-CNI data path; cleanup remains leak-free |
| Running snapshot | Native state, memory, writable disks and resource topology | Minimize pause; do not freeze the guest or hash/package payload on the hot path |
| Restore | Preserve identity and existing CNI resources for in-place restore | Preflight before replacing a live VM; restore must survive host-side runtime reconstruction |
| Clone | New VM identity, storage, vsock and all CNI resources; preserved CPU/memory/storage | Replace snapshot NICs with fresh NICs and configure the guest automatically |
| Hibernate | Atomic native capture followed by VMM termination | On persistence failure resume the source; on success release source memory immediately |
| Concurrency | Per-VM operation locks and shared immutable image/snapshot data | Unrelated VM operations run concurrently; no global serialization in create/restore |

P6 only covers the selected Cloud Hypervisor + OCI + CNI product path. The
reference runtime has additional capabilities, but those are future work and
must not expand the P6 performance target. The future backlog is explicit:

| Capability present in the reference runtime | KumaBox status | Planned parity stage |
| --- | --- | --- |
| Batch `start`, `stop`, `rm`, status and inspection | single-reference lifecycle is implemented | future |
| Disk attach/detach and managed data disks | not complete | future |
| Filesystem attach/detach (`virtio-fs`/vhost-user-fs equivalent) | not complete | future |
| Generic device attach/detach and VFIO | not complete | future |
| Network resize and multi-NIC lifecycle operations | base CNI and multi-NIC records exist; resize is not complete | future |
| Memory balloon, free-page reporting and hugepage policy | not complete as a user capability | future |
| Cloud-image/UEFI operational parity | compatibility path exists; needs command-level parity tests | future |
| Firecracker backend selection and lifecycle | explicitly excluded from P6 | future |

These are capability stages, not optional nice-to-haves. Their order is later
only because they do not belong in the first OCI+CNI startup measurement.

## 2. Product Priorities

When requirements conflict, use this order:

1. Isolation and correctness required to prevent cross-tenant corruption.
2. End-to-end ready latency and tail latency.
3. Host density and steady-state resource overhead.
4. Throughput under concurrent sandbox creation.
5. Portability, exhaustive synchronous verification and compatibility helpers.

This changes several defaults:

- A successful fast-path operation means the guest agent can execute a command, not merely that the VMM API socket exists.
- Checksums and durability work that do not protect the active host from corruption must leave the latency-sensitive path, become incremental, or run asynchronously.
- Clone identity and network setup remain automatic, but must not use long polling loops or full guest boot work.
- Features without benchmark evidence do not enter the default profile.
- The benchmark never reports restore-only time as full sandbox-ready time.

## 3. Current Progress

| Section | Status | Description |
| --- | --- | --- |
| P6-00 Parity Cleanup | done | Remove non-aligned snapshot modes and default-path compatibility work |
| P6-01 Default Path Parity | done | OCI + CNI is the default and compatibility paths remain explicit |
| P6-02 Snapshot and Restore Parity | done | Native capture, copy/ondemand/mmap restore, clone identity and first-exec readiness are implemented |
| P6-03 Command Readiness and Phase Metrics | done | Make `run` and `start` report first-exec readiness and record every user-visible phase |
| P6-04 End-to-End Benchmark Contract | single-VM baseline done, final comparison pending | One JSON benchmark for run, exec, snapshot, restore, clone and host fingerprints |
| P6-05 OCI Boot Critical Path | baseline done, optimization pending | Align kernel, initramfs, layers, overlay and agent startup with the default OCI path |
| P6-06 Block I/O Fast Path | single-VM baseline done, workload benchmark pending | Align COW/data disk queues, Direct I/O, sparse/reflink fallback and cache behavior |
| P6-07 Snapshot Capture Fast Path | baseline done, host acceptance pending | Keep the pause window short and report pause, staging and publication separately |
| P6-08 Restore and Clone Fast Path | baseline done, copy bottleneck confirmed | Keep native restore/clone ordering aligned and report copy/ondemand/mmap phase timings |
| P6-09 CNI Datapath Tuning | single-VM baseline done, datapath acceptance pending | Align CNI TAP queues, MTU, host queueing, GRO and virtio-net offloads |
| P6-10 Density and Concurrency | not measured | Measure parallel OCI+CNI startup and report batch wall time plus ready-time distribution |
| P6-11 Performance Regression Gate | not measured | Compare benchmark JSON against p50/p95 regression budgets |

### 3.3 Execution Rules

P6-03 is implemented first because the later work needs reliable phase data.
A preliminary single-VM comparison is now allowed before optimization so that
the next bottleneck is selected from measurements rather than assumptions.
P6-04 now has a reproducible single-VM baseline, but remains the final
product-to-product comparison after P6-05 through P6-10. P6-11 is still the
regression gate after that final baseline.

| Stage | Must be true before starting | Required evidence before marking done |
| --- | --- | --- |
| P6-03 | P6-00 through P6-02 complete | `run` and `start` expose VMM-ready, agent-ready and first-exec-ready; every phase has a monotonic timestamp |
| P6-05 | P6-03 complete | Boot-path measurements identify the dominant phase and the before/after result improves it without changing the default OCI+CNI contract |
| P6-06 | P6-05 complete | Storage behavior is aligned and measured across sparse allocation, reflink, fallback copy, read-only layers and writable COW |
| P6-07 | P6-06 complete | Snapshot capture reports pause duration separately from persistence and the source VM remains usable |
| P6-08 | P6-07 complete | Native restore and clone report disk stage, VMM restore, network identity and first-exec timings for all supported modes |
| P6-09 | P6-08 complete | CNI network readiness and datapath tests pass under the aligned queue/offload/MTU configuration, including cleanup |
| P6-10 | P6-09 complete | Concurrent startup and memory-density tests identify a safe concurrency limit and show no global serialization |
| P6-04 | P6-05 through P6-10 complete for the final run | One final command produces comparable JSON for cold, warm and concurrent cases, including image and host fingerprints |
| P6-11 | P6-04 complete | Baseline comparison fails on configured regressions and stores failure diagnostics |

### 3.4 Current Unified Host Baseline

The current KumaBox baseline was run three times on the reference Linux host
with 2 vCPU, 1 GiB memory, 64 MiB writable storage, CNI networking, and the
`p6-agent-image` image. The result is `/tmp/kumabox-p0/p6-baseline.json` and
uses schema `kumabox.p6.benchmark.v5`.

| Measurement | p50 | max | Interpretation |
| --- | ---: | ---: | --- |
| VMM API ready | 199 ms | 231 ms | Host-side VMM startup |
| guest overlay ready | 860 ms | 900 ms | Kernel/initramfs and OCI overlay assembly |
| guest systemd started | 1,021 ms | 1,066 ms | Overlay-to-systemd transition is 161 ms p50 |
| guest agent ready | 2,088 ms | 2,162 ms | Largest guest startup phase |
| guest multi-user target | 2,267 ms | 2,300 ms | 205 ms p50 after the agent |
| sandbox ready / agent ready | 3,314 ms | 3,317 ms | Application-visible startup boundary |
| first exec | 20 ms | 22 ms | Command execution after readiness |
| native snapshot | 1,493 ms | 1,816 ms | Current snapshot path |
| native pause | 1,215 ms | 1,411 ms | Pause remains a hot-path cost |
| native clone restore | 11,032 ms | 11,981 ms | Copy-mode clone is the dominant outlier |
| clone backend restore | 1,879 ms | 2,131 ms | Remaining time is staging and lifecycle work |
| portable restore | 402 ms | 422 ms | Separate from native clone readiness |
| stopped restore to ready | 3,150 ms | 3,156 ms | Comparable to a normal agent-ready boot |

This is a valid KumaBox baseline, but not yet a complete product comparison:
64 MiB was selected because the host did not have enough free space for the
reference runtime's 10 GiB minimum virtual COW disk. Snapshot and clone results
must be rerun with a matched storage shape before drawing a storage conclusion.

The separately observed reference-runtime cold sample was 249 ms to report the
VM running and 3.684 s to guest-agent readiness, using 2 vCPU and 1 GiB.
KumaBox measured 199 ms VMM readiness and 3.314 s agent readiness here. Startup
is therefore in the same order of magnitude, with KumaBox faster in this
sample. Snapshot and clone parity is not established because those reference
measurements have not yet been collected.

### 3.5 Earlier Preliminary Host Comparison

The first single-VM comparison was run on the reference Linux host before the
final parity baseline. KumaBox used `p3-agent-image-v3`, CNI networking, 512 MiB
memory and a 64 MiB writable overlay. Cocoon used an imported amd64 OCI rootfs,
CNI networking, 512 MiB memory and its minimum 10 GiB virtual COW disk. The
Cocoon rootfs was flattened during manual import, so these numbers identify the
next bottleneck but are not the final product-to-product claim.

| Measurement | KumaBox | Cocoon |
| --- | ---: | ---: |
| `run`/VMM command return | about 5.76 s | 249 ms |
| guest agent ready | about 5.73 s | 3.684 s |
| first guest exec | included in KumaBox readiness boundary | included in Cocoon readiness probe |
| native snapshot | 1.353 s p50 | not measured in this run |
| native clone, copy mode | 11.735 s p50 | not measured in this run |

The comparison shows two immediate priorities:

1. KumaBox's default OCI boot path adds roughly 2.0 s before guest-agent
   readiness, even after the VMM has been started. P6-05 must attribute that
   time across kernel, initramfs, EROFS/overlay assembly, userspace services and
   agent startup before changing the image or service graph.
2. KumaBox native clone in `copy` mode is dominated by memory and writable-disk
   copying. The copy path remains required for portability, but the benchmark
   must measure `ondemand` and `mmap` separately and should not treat full copy
   as the high-density default when the snapshot can remain pinned locally.

The final comparison must use the same image content, vCPU/memory/storage
shape, CNI network, readiness boundary and host fingerprint. The preliminary
numbers are therefore a prioritization signal, not a release target.

### 3.1 Command-Oriented Parity Audit

The implementation order is decided from the user-visible command lifecycle,
not from package names. The following audit compares the current KumaBox
behavior with the Cloud Hypervisor path in the reference runtime. It records
whether a difference is a real P6 gap, a later capability, or merely a CLI
shape difference.

#### `run IMAGE`

KumaBox currently performs:

```text
resolve OCI image
  -> create VM record
  -> allocate CNI netns, veth/TAP and IP
  -> create sparse COW and prepare backing storage
  -> render Cloud Hypervisor config
  -> launch Cloud Hypervisor
  -> wait for API socket
  -> mark VM running
```

The reference runtime performs the same major sequence: create the VM record,
allocate CNI resources, build the direct-boot configuration, launch Cloud
Hypervisor in the VM network namespace, wait for its API socket and mark the VM
running. Its `run` handler also separates creation from start, which makes
rollback and start failures explicit.

| Comparison | Result | P6 decision |
| --- | --- | --- |
| OCI direct boot and shared read-only layers | aligned | keep as the default |
| CNI network allocation before VMM launch | aligned | measure, then optimize |
| API socket as the returned success point | both currently do this | **P6 gap:** add VMM-ready, agent-ready and first-exec milestones; do not claim full ready at API socket |
| Full rootfs extraction on every run | neither default path requires it | keep out of the hot path |
| Batch `run` | reference also accepts one image per command | no gap |

The important point is that the reference runtime does not magically make the
guest ready when the VMM socket appears. For our product contract, both sides
still need an explicit first-command measurement. This is the first run-path
item to implement before tuning individual syscalls.

#### `start VM`

Both implementations reuse the existing VM record, render or refresh the VMM
configuration, start Cloud Hypervisor and persist the process identity. Both
retain the network identity across stop/start.

| Difference | Classification |
| --- | --- |
| KumaBox accepts one VM reference; the reference runtime accepts multiple references and routes them to backends | CLI/throughput feature, not single-VM latency |
| KumaBox currently returns after VMM start and observation | **P6 gap:** use the same first-exec readiness contract as clone/restore |
| Reference runtime has backend routing for Cloud Hypervisor and Firecracker | Firecracker is explicitly deferred to P9 |

#### `stop VM`

The common sequence is:

```text
lock VM
  -> reconcile observed process state
  -> resume if paused
  -> request VMM shutdown
  -> wait for graceful exit
  -> SIGTERM/SIGKILL fallback
  -> remove runtime socket and pid files
  -> persist stopped state
```

KumaBox and the reference runtime both retain CNI resources after stop so that
the same VM can start again with the same identity. This is correct for a fast
restart path.

The reference runtime supports stopping a batch of VMs and has an explicit
ACPI path for UEFI guests. KumaBox's direct OCI path uses the Cloud Hypervisor
shutdown API and process fallback; UEFI/cloud-image compatibility remains
separate. There is no P6 single-VM stop correctness gap. Batch stop can be
added later for operational throughput.

#### `delete VM` / `vm rm`

Both implementations refuse to delete a running VM unless force is requested,
then stop the VMM, delete CNI resources, remove the netns/TAP bookkeeping,
remove managed runtime files and delete the VM record. Both preserve a cleanup
pending record when provider cleanup fails so retry is possible.

The reference runtime accepts multiple VM references in one command. KumaBox
currently accepts one. This is a management API difference, not a microVM
startup bottleneck, so it is not ahead of P6's hot path.

#### `exec VM -- COMMAND`

Both use a guest agent over vsock. The host checks that the VM is running, sends
the command, returns stdout/stderr and preserves the guest exit code.

KumaBox now also uses a synthetic `true` exec as the readiness probe after
native restore and clone. The remaining gap is normal `run` and `start`: they
still publish `running` after the VMM is ready rather than after this same
agent-level check. That is a direct P6-03/P6-05 dependency.

#### `snapshot create --type running` / `snapshot save`

The aligned running-snapshot sequence is:

```text
lock VM
  -> pause Cloud Hypervisor
  -> capture VMM config, device state and memory
  -> capture writable COW/data disks
  -> resume the source VM
  -> verify and publish the snapshot record
```

KumaBox and the reference runtime both keep the pause window around the VMM
capture and writable-disk capture. Neither uses guest filesystem freeze in the
default native path. Portable checksums, packaging and compression happen
outside the local snapshot readiness path.

Status: **P6-02 aligned.** Remaining work is performance measurement: pause
duration, disk method selected, snapshot publish time and source resume time.

#### `snapshot create --type disk` / stopped snapshot

Both support a stopped, disk-oriented snapshot that copies or links writable
state without capturing a running VMM. It is useful for portable restore but is
not the low-latency running-agent snapshot path.

KumaBox keeps portable export/import as explicit `snapshot export` and
`snapshot import` operations, matching the reference runtime's separate
portable archive flow. Neither should be added to `snapshot create --type
running`.

#### `snapshot restore` / `vm restore`

There are two distinct operations and they must not be conflated:

1. **In-place native restore:** replace the original VM's memory, VMM state and
   writable disks, preserve its vsock/network identity, then resume it.
2. **Portable stopped restore:** create a new VM record and reconstruct its
   writable disks from a portable snapshot package.

The reference runtime has both forms. KumaBox has both forms as well. Native
restore supports `copy`, `ondemand` and `mmap`, validates host compatibility,
stages writable disks with bounded parallelism, and now requires the restored
agent to complete an exec before publishing success.

Status: **P6-02 aligned.** The next work is not another restore feature; it is
measuring and reducing the time spent in disk staging, VMM restore, agent
reconnect and first exec.

#### `clone SNAPSHOT` / `vm clone`

Both clone paths preserve the workload state but allocate new runtime identity:

```text
read and verify native snapshot
  -> create fresh VM record and writable storage
  -> allocate fresh vsock and CNI resources
  -> restore native VMM state
  -> replace guest hostname, MAC, IP and routes
  -> resume and verify the first exec
```

The reference runtime additionally regenerates cloud-init/cidata for non-direct
boot images, supports data-disk cloning and performs NIC replacement during the
restore transaction. KumaBox's default OCI direct-boot path does not need
cidata, and its optional data-disk/device hotplug work is intentionally outside
the P6 default path.

Status: **default OCI clone is aligned.** The remaining performance questions
are whether disk clone uses reflink, memory mode and CNI setup in the cheapest
order, and how much of the restore can overlap without increasing tail
latency.

#### `hibernate VM` / `vm hibernate`

Both atomically capture a native running snapshot, persist it, terminate the
VMM and leave the VM stopped with a resume snapshot reference. If persistence
fails, both attempt to resume the source VM. This is aligned and is not the
first P6 optimization target; it matters mainly for memory density.

#### `network` and `--network`

The reference runtime selects a CNI network with repeatable `--network` flags
and gives each VM a netns, veth/TAP path, IP and route. KumaBox now defaults to
the equivalent `cni:default` path and keeps host-tap as an explicit compatibility
mode.

The remaining P6 network work is datapath performance and readiness measurement:
CNI ADD time, TAP creation, tc redirect, guest link/address/route readiness,
queue count, offloads and cleanup latency. It is not a reason to switch back to
host-tap for the default path.

#### Capability differences that are not P6 hot-path blockers

The reference runtime exposes more operational commands, including batch VM
operations, disk/filesystem/device attach and detach, network resize, memory
balloon controls and a Firecracker backend. KumaBox does not yet have all of
these.

They are real capability differences, but they do not block the P6 product
goal of a fast OCI+CNI sandbox. They should be tracked separately from the
latency path. Firecracker remains deferred to P9; device hotplug and similar
flexibility work remains post-P6 unless a concrete agent workload proves it is
required.

### 3.2 P6 Order Derived From the Audit

The command comparison changes the implementation order to:

1. **P6-03:** add phase timestamps and first-exec readiness to `run` and
   `start`, because the current success point is only the VMM API socket.
2. **P6-05:** align and optimize the OCI boot path using the measured phases:
   kernel,
   initramfs layer mount, overlay mount, userspace and agent startup.
3. **P6-06/P6-07:** align and optimize block I/O and snapshot capture based on measured
   disk staging and pause time.
4. **P6-08:** align and optimize native restore/clone ordering and copy/reflink/memory
   mode selection using the phase measurements.
5. **P6-09/P6-10:** align and tune CNI datapath and concurrent density after single-VM
   readiness is measured.
6. **P6-04:** only after P6-05 through P6-10 are complete, run the final
   KumaBox-versus-Cocoon benchmark for the equivalent command paths.
7. **P6-11:** enforce the final measured budgets as a regression gate.
This ordering makes the missing behavior visible before any low-level tuning:
we first measure the complete command as the user experiences it, then optimize
the phase that dominates the result, and only then close the remaining parity
gaps. Capabilities listed as future work are
deliberately not used to block P6 completion.

## 4. Measurement Contract

Every result records the host, kernel, VMM version, CPU model, filesystem, image digest, vCPU count, memory, COW size, network mode and cache state. Results from different environments are not compared as regressions.

### 4.1 Startup milestones

```text
commandStart
imageResolved
storageReady
networkReady
vmmSpawned
vmmAPIReady
kernelStarted
rootMounted
agentConnected
firstExecCompleted
```

Primary metric:

```text
sandboxReadyMs = firstExecCompleted - commandStart
```

Supporting metrics:

- `controlPlaneMs`
- `storagePrepareMs`
- `networkPrepareMs`
- `vmmAPIReadyMs`
- `kernelToAgentMs`
- `firstExecMs`

### 4.2 Snapshot milestones

```text
vcpusPaused
backendSnapshotCompleted
writableDisksStaged
vcpusResumed
snapshotPublished
integrityCompleted
```

Primary metrics:

- `snapshotPauseMs`: application-visible stop time.
- `snapshotReadyMs`: time until the snapshot can be restored locally.
- `snapshotPortableMs`: time until strict verification and portable export readiness.

Local restore readiness must not wait for work required only by portable export.

### 4.3 Restore and clone milestones

```text
restoreStarted
payloadStaged
vmmSpawned
backendRestored
vcpusResumed
agentConnected
identityApplied
networkReachable
firstExecCompleted
```

Primary metric:

```text
restoreReadyMs = firstExecCompleted - restoreStarted
```

`backendRestoredMs` is diagnostic only and cannot be used as the product restore number.

### 4.4 Sampling

- At least 3 warm-up runs and 20 measured runs.
- Report p50, p95, minimum, maximum and failures.
- Cold and warm page-cache results are separate suites.
- Single-VM and concurrent runs are separate suites.
- A failed run remains in the failure-rate denominator.

## 5. Default Performance Profile

The default profile is named `agent-fast` and has these semantics:

- OCI image only.
- Cloud Hypervisor direct boot.
- CNI networking unless `--network none` is explicit.
- One content-addressed EROFS device per retained layer until layer compaction is benchmarked.
- Sparse ext4 COW with no eager full-disk initialization.
- Guest agent enabled and started before nonessential services.
- No SSH, package timers, getty or graphical target on the readiness path.
- Configurable compatibility profile keeps full Ubuntu services for interactive VM users.

The default CLI behavior is equivalent to selecting OCI and the default CNI conflist. Host-tap and cloud-image paths require explicit selection.

## 6. Task Breakdown

### P6-00: Parity Cleanup

Status: done.

Remove guest freeze/thaw from the product snapshot path, CLI acceptance and guest-agent capability contract. Keep portable snapshot export as a separate offline command, but remove any automatic promotion, hashing or packaging from native local capture and restore.

Acceptance:

- Native snapshot has one default crash-consistent mode.
- No freeze/thaw RPC is sent or advertised by the default agent.
- The consolidated E2E suite contains no filesystem-consistency stage.
- Cloud image, host-tap and UEFI code is not initialized for an OCI+CNI run.

### P6-01: Default Path Parity

Status: in progress.

The first parity step makes the path used by an OCI image match the intended
production path: CNI is selected automatically when no `--network` is given.
`--network none`, `--network host-tap`, and an explicit `cni:NAME` remain
available as deliberate choices. Cloud-image compatibility does not silently
inherit the OCI default.

Acceptance:

- A plain OCI `run IMAGE` selects `cni:default` when `network.mode = "cni"`.
- `--network none` still disables networking.
- Host-tap remains available only when explicitly selected or configured.
- The rendered VM record and VMM config show the selected CNI provider.

### P6-02: Snapshot and Restore Parity

Match the reference runtime's native snapshot lifecycle before measuring speed.
Running snapshots pause the VMM, capture memory/device state and writable disks,
then resume. Restore and clone use the selected Cloud Hypervisor memory mode;
clone gets new storage, vsock and CNI identity while in-place restore keeps the
existing VM identity.

Acceptance:

- Native snapshot and hibernate do not call the guest agent.
- Restore supports `copy`, `ondemand` and `mmap` with explicit capability checks.
- Clone allocates fresh storage, vsock and CNI resources.
- A restored or cloned VM is only reported ready after the agent can execute.

### P6-03: Runtime Phase Metrics

Status: **implemented.** `run` and `start` now persist the same lifecycle milestones in the VM record: command start, image resolution, network, storage, VMM spawn, VMM API readiness, guest-agent connection, and first successful guest exec. Direct OCI boots use the first successful agent exec as the readiness boundary; non-direct compatibility boots are not forced through that gate. The record also stores the image digest, host/runtime fingerprint, and total ready duration.

Add structured phase events inside KumaBox. Shell timing alone is insufficient because it cannot attribute delays.

Acceptance:

- Timings use a monotonic clock.
- The image digest and environment fingerprint are included.
- The result distinguishes VMM-ready, agent-ready, network-ready and exec-ready.
- The harness can compare a baseline file and fail on a configured regression budget.

### P6-04: Benchmark Contract

The benchmark entry point now produces machine-readable JSON and covers
startup, snapshot, restore and clone for a single VM. It cleans up resources,
retains failure diagnostics, records guest boot milestones, and rejects failed
startup instead of publishing partial timings. A final product comparison
still waits for matched storage and reference-runtime snapshot/clone data.

Acceptance:

- Timings use a monotonic clock.
- The image digest and environment fingerprint are included.
- The result distinguishes VMM-ready, agent-ready, network-ready and exec-ready.
- The harness can compare a baseline file and fail on a configured regression budget.

### P6-05: OCI Boot Critical Path

Status: **baseline captured; optimization gap remains.** The OCI boot path now
uses the Cocoon-aligned kernel command line, root assembly, network fallback,
agent service setup and reduced initramfs module set. No custom fast boot target
was added; readiness remains based on the existing agent and first-exec path.

The unified three-run baseline measured 3.314 s p50 from VMM launch to
guest-agent readiness. The guest milestones were overlay 860 ms, systemd
1,021 ms, agent 2,088 ms, and multi-user 2,267 ms. The separately observed
reference-runtime sample reached guest-agent readiness in 3.684 s with the
same 2-vCPU/1-GiB shape. The numbers are now comparable for startup order, but
the image contents and host conditions still need a controlled repeat before
setting an optimization target.

Profile kernel, initramfs, root assembly, userspace and agent startup. Remove services not required by an agent sandbox from `agent-fast`.

Alignment note: the initramfs and agent primary path follows Cocoon's
implementation. KumaBox keeps one explicit compatibility extension: some
Cloud Hypervisor versions used by the reference host do not expose virtio
disk serials to the guest, so the existing attach-order fallback remains after
the serial lookup. Removing it would make the selected OCI path fail before
the agent can start. The unused COW control bind mount and boot-time
`machine-id` rewrite have been removed to match Cocoon's root assembly path.

Remaining work:

- Minimize initramfs contents and decompression cost.
- Eliminate per-device one-second polling and serial fallback delays.
- Measure layer count and mount cost; compact layers only with evidence.
- Measure the remaining userspace services before considering any image change.
- Preserve the full compatibility image while validating the default image.

Acceptance:

- Console timestamps explain at least 95% of kernel-to-agent time.
- No fixed sleep exists on the successful boot path.
- Agent readiness does not depend on `multi-user.target` or `graphical.target`.

### P6-06: Block I/O Fast Path

Status: **baseline captured; workload benchmark pending.** Cloud Hypervisor disk
arguments now follow the Cocoon policy: read-only EROFS uses page cache;
writable disks use configurable queues, queue affinity, sparse raw handling and
Direct I/O by default. Linux copy paths already prefer reflink, then sparse
copy, then streaming copy, with bounded concurrency.

Implement benchmark-driven virtio-blk settings:

- `num_queues` derived from vCPU count and host limits.
- Configurable `queue_size`, with a measured default.
- Queue affinity for writable disks when beneficial.
- Direct I/O policy for writable disks.
- Sparse and reflink-aware COW handling.
- Explicit read-only EROFS and writable COW profiles.
- Read-only disks default to host page cache; writable disks default to direct I/O when the host filesystem supports it.

Acceptance:

- fio covers sequential bandwidth, random IOPS and mixed latency.
- The selected defaults improve representative agent workloads, not only synthetic throughput.
- Direct I/O fallback is explicit when the backing filesystem does not support it.

### P6-07: Snapshot Capture Fast Path

Status: **fast path implemented; host acceptance pending.** Running snapshot
capture stages writable disks during the single pause window, resumes the VM,
and publishes the local snapshot without a second full read for `fsync` and
SHA256. This follows the reference runtime's `NoSync` local-copy path. Snapshot
records still persist separate pause, native capture, disk staging,
publication and total timings.

Strict payload hashing remains on stopped snapshots and explicit integrity
work. A fast running snapshot records payload shape and topology immediately;
`snapshot verify` can validate its inventory and size, while portable export
and any future durable/off-host path must perform the full checksum step before
publishing external data. The native snapshot E2E continues to verify that the
source guest remains usable after capture.

Keep the pause window limited to the Cloud Hypervisor native snapshot transaction and writable-disk reflink/staging. Hashing, package compression and full-tree sync are outside the pause window and outside local restore readiness.

The native local snapshot is the default. Portable packaging is a separate command and lifecycle, not a stricter mode selected on the hot path.

Acceptance:

- Snapshot pause time is reported independently from publication time.
- Reflink-capable filesystems avoid copying COW contents.
- Memory and disk hashing never occurs while vCPUs are paused.
- The default path never calls guest freeze/thaw.

### P6-08: Restore and Clone Fast Path

Optimize and compare `copy`, `ondemand` and `mmap` restore modes. Avoid copying or hashing the full memory image before local restore when snapshot ownership and immutable leases already establish trust.

The unified host benchmark measured native clone in `copy` mode at 11.032 s
p50, with backend restore at 1.879 s. The remaining time is
primarily staging the snapshot's memory and writable disk data. This makes
non-copy local restore modes a performance priority for high-density agents;
the portable copy mode remains available for cases that require an independent
snapshot-owned payload.

Acceptance:

- Non-copy modes pin their source snapshot for the complete VM lifetime.
- Copy mode uses reflink or sparse copy before streaming fallback.
- Guest identity is supplied through an idempotent fast agent request.
- Network readiness uses event/state confirmation rather than long blind retries.
- First exec succeeds when the command returns success.

Implementation status:

- `copy` still copies memory payloads, while `ondemand` and `mmap` link the immutable
  snapshot memory files and pin the snapshot through `SnapshotDependency`.
- Writable disks are staged with the existing reflink/sparse/buffered copy chain and
  committed before the VMM restore, matching the direct restore order.
- VM `lastRestore` now records native staging, disk staging, disk commit, backend
  restore, identity, readiness, and total durations.
- The snapshot E2E checks that clone timing data is published only with the final
  running VM record.

### P6-09: CNI Datapath Tuning

The default network remains CNI:

```text
virtio-net <-> multiqueue TAP <-> tc redirect <-> CNI link
```

Tune and expose:

- aligned `num_queues = 2 * vCPU` and queue size 512 as the initial benchmark baseline.
- TSO/UFO/checksum offload and TAP VNET_HDR.
- TAP and CNI-link MTU synchronization.
- CNI-side MAC passthrough to the guest NIC.
- tx queue length where measurements justify it.
- CNI setup caching that does not reuse tenant identity.

Acceptance:

- iperf3 throughput, small-message latency and packet rate are recorded.
- CNI DEL, netns, TAP, tc and IPAM cleanup remain leak-free.
- Network setup time is included in sandbox-ready latency.

Implementation status:

- The default CNI TAP path now uses the same 512-entry virtio-net queue depth
  as the reference runtime.
- TAP devices enable VNET_HDR, use a 10000-packet transmit queue and best-effort
  GRO aggregation at 65536 bytes.
- CNI-provided MTU and MAC remain authoritative and are applied to the TAP and
  Cloud Hypervisor device.
- Cloud Hypervisor receives TSO, UFO and checksum offload settings on the
  default virtio-net path.

### P6-10: Density and Concurrency

Status: **not measured yet.** The current baseline intentionally used one VM
at a time (`concurrency=1`); it says nothing about high-density startup or
global serialization.

Measure and optimize:

- idle RSS/PSS per VM.
- page-cache sharing across EROFS layers and mmap restores.
- balloon and free-page-reporting behavior.
- hugepage auto-detection and its density/latency trade-off.
- 10, 50 and 100 concurrent sandbox creation where the host allows it.
- CPU, memory, file descriptor, TAP and netns limits.

Acceptance:

- Density tests report successful sandboxes per host resource unit.
- Parallel creation does not serialize on a global VM lock.
- Image and snapshot stores avoid duplicate large-file reads.
- Host pressure produces controlled admission failure instead of thrashing.

### P6-11: Performance Regression Gate

Status: **not measured yet.** The baseline JSON is recorded, but no versioned
threshold comparison has been run against it.

Store versioned benchmark baselines outside functional fixtures. Gates apply to identical environment fingerprints.

Initial gate policy:

- No more than 10% p50 regression.
- No more than 15% p95 regression.
- No new fixed-delay step on a successful path.
- No increase in failure rate.
- Any intentional regression requires an explicit benchmark justification.

Absolute targets are set only after P6-01 establishes a reproducible baseline on the reference host. Targets must cover both latency and density; optimizing one by consuming unbounded host memory is not accepted.

Implementation status:

`scripts/linux/benchmark-p6.sh` is the single benchmark entry point. It writes a
versioned JSON record containing the image and host fingerprint, sequential
lifecycle samples, and an optional parallel startup batch. Passing `--baseline`
compares the supported latency metrics and fails when p50 regresses by more than
10 percent or p95 by more than 15 percent.

## 7. Explicit Non-Goals

The following are not capability parity requirements for the current product
contract:

- Windows guests.
- Cross-VMM snapshot compatibility for one snapshot format.
- A global control daemon.
- Marketing numbers based only on VMM API readiness.

VFIO, virtio-fs, general hotplug, Firecracker, Windows and other non-default
backend/guest capabilities are explicitly deferred. They are recorded in the
future backlog, but do not block P6 completion.

## 8. Completion Criteria

P6 is complete when:

- The capability parity contract is covered by end-to-end tests.
- OCI + CNI is the tested and documented default.
- Startup, snapshot and restore have internal phase metrics.
- The benchmark suite produces reproducible p50/p95 results.
- Block and network queue defaults are benchmark-derived.
- Snapshot local-ready no longer waits for portable-only integrity work.
- Restore/clone reports ready only after first exec succeeds.
- Concurrent density and memory overhead are measured.
- A regression gate protects the selected baseline.
- Every command in the selected Cloud Hypervisor + OCI + CNI scope has an
  implemented KumaBox equivalent and an end-to-end acceptance test.
- Deferred capabilities are listed in the future backlog and are not silently
  confused with P6 completion.
