<p align="center">
  <img src="assets/logo.png" alt="KumaBox logo" width="180">
</p>

<!-- <h1 align="center">KumaBox</h1> -->

<p align="center">
  A daemonless microVM sandbox runtime for agents, automation, and untrusted workloads.
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go&logoColor=white" alt="Go 1.22+">
  <img src="https://img.shields.io/badge/platform-Linux-FCC624?logo=linux&logoColor=black" alt="Linux">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-green.svg" alt="MIT License"></a>
  <a href="https://github.com/kgpp34/KumaBox/actions/workflows/ci.yml"><img src="https://github.com/kgpp34/KumaBox/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <img src="https://img.shields.io/badge/status-active%20development-orange" alt="Active development">
</p>

KumaBox runs each sandbox inside a hardware-virtualized microVM while keeping the workflow close to a container CLI. It manages images, VM lifecycle, networking, snapshots, guest execution, logs, and local runtime state without requiring a long-running control daemon.

> [!IMPORTANT]
> KumaBox is under active development. It is intended for Linux/KVM development and evaluation environments; review the [security model](#security-model) before using it with hostile workloads.

## Why KumaBox

Agents and automation routinely execute generated code, install packages, access networks, and process user-supplied files. Containers are fast and convenient, but share the host kernel. KumaBox adds a microVM boundary while preserving a local, CLI-first operating model.

- **Hardware-backed isolation** — each workload runs in a Cloud Hypervisor microVM on KVM.
- **Daemonless control plane** — persisted state is reconciled with the observed VMM process state.
- **Reproducible storage** — images, VM metadata, logs, snapshots, leases, and content are stored under explicit roots.
- **OCI-first images** — resolve and build digest-pinned OCI images into shared EROFS layers with per-VM COW; cloud images remain a compatibility path.
- **CNI-first networking** — per-VM netns, multiqueue TAP and tc redirect by default; host TAP and no-network modes remain explicit alternatives.
- **Snapshot lifecycle** — capture, verify, export, import, restore, and clone snapshots.
- **Automation-friendly output** — operational commands expose structured JSON where applicable.

## Architecture

```text
                         +----------------------+
                         |     kumabox CLI      |
                         +----------+-----------+
                                    |
              +---------------------+---------------------+
              |                     |                     |
       +------v------+       +------v------+       +------v------+
       | State stores |       | Image/OCI   |       | Networking  |
       | VM/snapshot  |       | pipelines   |       | TAP or CNI  |
       +------+-------+       +------+------+       +------+------+
              |                      |                     |
              +----------------------+---------------------+
                                     |
                          +----------v-----------+
                          |  Cloud Hypervisor    |
                          |  microVM + guest     |
                          |  agent over vsock    |
                          +----------------------+
```

The CLI is the control plane. Durable records allow later commands to inspect and reconcile resources even if a previous CLI or VMM process exited unexpectedly. Cloud Hypervisor is currently the supported VMM backend.

## Requirements

| Component | Requirement |
| --- | --- |
| Host | Linux with hardware virtualization enabled |
| Virtualization | KVM available at `/dev/kvm` with read/write permission |
| Go | Go 1.22 or newer (building from source) |
| VMM | `cloud-hypervisor` available on `PATH` or supplied by flag/config |
| Disk tooling | `qemu-img` available on `PATH` or supplied by flag/config |
| Host networking | `/dev/net/tun`, `ip`, root privileges, and `iptables` or `nft` |
| CNI networking | CNI configuration under `/etc/cni/net.d` and plugins under `/opt/cni/bin` for the default network path |

Run the built-in preflight check before creating a VM:

```bash
sudo ./bin/kumabox doctor
```

For a stricter development-host check, including networking:

```bash
sudo scripts/linux/env-check.sh --strict --network
```

For a standalone check on a fresh Linux host, download the KumaBox doctor:

```bash
curl -fsSL -o kumabox-doctor https://raw.githubusercontent.com/kgpp34/KumaBox/master/scripts/linux/kumabox-doctor.sh
install -m 0755 kumabox-doctor /usr/local/bin/
sudo kumabox-doctor --upgrade
kumabox-doctor
```

`--upgrade` installs the host packages, Cloud Hypervisor, UEFI firmware, and
the CNI plugins required by the default networking path. Use `--json` for
automation and set `KUMABOX_*` environment variables to pin versions or
override installation paths.

## Getting Started

### 1. Build

```bash
git clone https://github.com/kgpp34/KumaBox.git
cd KumaBox
make build
./bin/kumabox version
```

The binary is written to `bin/kumabox`.

### 2. Build an OCI VM image

Resolve an OCI image and publish a managed direct-boot image:

```bash
sudo ./bin/kumabox image build docker.io/library/ubuntu:24.04 \
  --name ubuntu-oci \
  --platform linux/amd64
```

Verify the registered image:

```bash
sudo ./bin/kumabox image ls
sudo ./bin/kumabox image inspect ubuntu-oci --json
```

### 3. Run a sandbox

```bash
sudo ./bin/kumabox run ubuntu-oci \
  --name devbox \
  --cpus 2 \
  --memory 1G
```

Inspect the runtime and read its logs:

```bash
sudo ./bin/kumabox ps
sudo ./bin/kumabox inspect devbox --json
sudo ./bin/kumabox logs devbox
```

If the image contains `kumabox-agent`, commands can be executed inside the running guest:

```bash
sudo ./bin/kumabox agent ping devbox
sudo ./bin/kumabox exec devbox -- uname -a
```

### 4. Stop and clean up

```bash
sudo ./bin/kumabox stop devbox
sudo ./bin/kumabox delete devbox
sudo ./bin/kumabox gc
```

Use `kumabox <command> --help` for the complete flags and examples supported by a command.

## Capability Matrix

| Area | Capabilities |
| --- | --- |
| VM lifecycle | Create, run, start, stop, pause, resume, inspect, list, delete |
| Images | Pull, resolve, and build OCI images by default; import/pull cloud images as compatibility assets |
| Guest operations | Agent readiness checks and command execution over vsock |
| Networking | CNI netns/TAP/tc by default; explicit no-network or managed host TAP; inspect/setup/teardown |
| Snapshots | Stopped-disk and native running snapshots; verify, export, import, restore, clone |
| Operations | Doctor checks, logs, state reconciliation, garbage-collection inspection |
| Automation | JSON output on inspection and other machine-oriented command paths |

## Common Commands

```text
kumabox doctor                       Check host requirements
kumabox image <command>              Manage cloud and OCI images
kumabox run [IMAGE]                  Create and start a VM
kumabox ps                           List VM records
kumabox inspect VM                   Inspect a VM record
kumabox exec VM -- CMD [ARG...]      Execute a command in a guest
kumabox logs VM                      Read VM logs
kumabox pause|resume VM              Control a running VM
kumabox snapshot <command>           Manage stopped and native snapshots
kumabox clone SNAPSHOT               Clone a native snapshot with a new identity
kumabox restore VM SNAPSHOT          Restore a native snapshot to its original VM
kumabox network <command>            Manage and inspect network resources
kumabox stop VM                      Stop a VM
kumabox delete VM                    Delete a VM
kumabox gc                           Inspect or remove unreferenced resources
```

## Configuration

KumaBox loads built-in defaults, optionally overlays a TOML file supplied with `--config`, and finally applies CLI overrides.

```toml
[runtime]
root_dir = "/var/lib/kumabox"
run_dir = "/run/kumabox"
log_dir = "/var/log/kumabox"

[backend.cloud_hypervisor]
binary = "cloud-hypervisor"
api_socket_timeout_ms = 5000
stop_timeout_ms = 10000

[storage]
qemu_img_binary = "qemu-img"

[network]
mode = "cni"
default = "cni"
bridge = "kumabox0"
cidr = "10.88.0.0/16"
gateway = "10.88.0.1"
dns = ["1.1.1.1", "8.8.8.8"]
tap_prefix = "kbtap"
nat_backend = "auto"
cni_config_dir = "/etc/cni/net.d"
cni_bin_dir = "/opt/cni/bin"
```

Example:

```bash
sudo ./bin/kumabox --config /etc/kumabox/config.toml doctor
```

The runtime directories can also be overridden with `--root-dir`, `--run-dir`, and `--log-dir`. Backend tool paths can be overridden with `--cloud-hypervisor-bin` and `--qemu-img-bin`.

## State and Data

The default filesystem layout is:

| Path | Purpose |
| --- | --- |
| `/var/lib/kumabox` | Durable images, VM records, snapshots, network leases, and content |
| `/run/kumabox` | Ephemeral PID files, API sockets, and rendered runtime configuration |
| `/var/log/kumabox` | VM and runtime logs |

Use separate root directories when isolating development environments or test runs. Do not modify state files while KumaBox commands or managed VMs are active.

## Development and Verification

Run the standard build and unit-test suite:

```bash
make build
make test
go vet ./...
```

The Linux verification suite exercises the runtime in phased scenarios:

```bash
sudo scripts/linux/verify.sh
```

The consolidated scripts cover environment, OCI, CNI and snapshot/runtime behavior. Most runtime verification requires Linux, KVM, Cloud Hypervisor, CNI plugins and prepared boot assets.

## Security Model

KumaBox provides a stronger workload boundary than a shared-kernel container by placing the guest behind KVM and Cloud Hypervisor. This does not make arbitrary workloads risk-free:

- Host networking setup changes privileged interfaces, routes, and NAT rules.
- VM images, kernels, firmware, snapshot packages, and OCI content remain part of the trusted computing base.
- Guest-to-host integrations such as vsock and the guest agent should expose only the minimum required functionality.
- Production deployments should apply host hardening, resource limits, artifact verification, and independent security review.

## Project Status

KumaBox is actively developed and does not yet promise a stable CLI, configuration schema, state format, or snapshot compatibility contract across releases. Pin the exact revision when integrating it into automation, and validate upgrades against disposable state before applying them to important workloads.

## Contributing

Issues and pull requests are welcome. Before submitting a change:

1. Keep the change focused and document user-visible behavior.
2. Add or update tests for the affected package.
3. Run `make test` and `go vet ./...`.
4. Run the relevant Linux verification scripts for runtime-facing changes.

## License

KumaBox is available under the [MIT License](LICENSE).
