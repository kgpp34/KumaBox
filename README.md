<p align="center">
  <img src="assets/logo.png" alt="KumaBox logo" width="144">
</p>

<h1 align="center">KumaBox</h1>
<p align="center">A Linux microVM runtime for AI agents.</p>
<p align="center"><b>English</b> · <a href="README.zh-CN.md">简体中文</a></p>
<p align="center">
  <a href="https://github.com/kgpp34/KumaBox/actions/workflows/ci.yml"><img src="https://github.com/kgpp34/KumaBox/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-green.svg" alt="MIT License"></a>
</p>

KumaBox runs agent workloads in KVM-isolated microVMs backed by Cloud Hypervisor.
It manages images, sandbox lifecycle, guest operations, networking, storage and
snapshots on a single host, through a CLI and HTTP API.

## Capabilities

| Area | Supported operations |
| --- | --- |
| Images | Pull from registries; import Docker archives and OCI layouts or archives; verify, inspect and remove images. |
| Sandboxes | Create, run, start, stop and remove; configure CPU, memory and storage; query state and logs. |
| Guest operations | Execute commands, manage background processes, transfer files, attach a console and forward ports. |
| Snapshots | Save, restore, clone and hibernate; export and import snapshots for transfer between compatible hosts. |
| Networking | TAP/eBPF networking with fixed guest identity and reusable TAP devices; CNI and Linux bridge backends. |
| Storage and devices | Persistent writable root disks, data disks, virtio-fs directory sharing and VFIO PCI passthrough. |
| Resources | cgroup controls, hugepages, KSM and memory ballooning. |

## Architecture

CLI commands submit operations to a resident `kumabox daemon` over a local Unix
socket. The daemon owns the metadata store, coordinates resource operations and
supervises VMM processes. `kumabox serve` provides an authenticated HTTP API.
Metadata is stored locally in SQLite.

Each sandbox has a dedicated guest kernel and VMM process. Read-only EROFS image
layers are shared; each sandbox has a private writable ext4 disk. The Rust guest
init assembles the root filesystem and supervises the Go agent, which handles
guest commands and file operations over vsock.

[KumaBoxd](https://github.com/kgpp34/KumaBoxd) builds on this runtime to manage
node-local templates, warm pools and sandbox leases.

## Getting started

Running sandboxes requires Linux, KVM (`/dev/kvm`) and root privileges. Download
host tools and the matching guest image from
[Releases](https://github.com/kgpp34/KumaBox/releases).
Guest images must include compatible boot artifacts and the KumaBox agent.

To build from source, use Go 1.24.4 or newer:

```bash
make build
sudo ./bin/kumabox-check --upgrade
sudo ./bin/kumabox daemon
```

With the daemon running, import a compatible guest image archive in another
terminal and create a sandbox:

```bash
sudo ./bin/kumabox image import demo-image /path/to/guest-image.tar
sudo ./bin/kumabox run demo-image --name demo --cpus 2 --memory 512MiB --nics 0
sudo ./bin/kumabox exec demo -- uname -a
sudo ./bin/kumabox snapshot save demo --name golden
sudo ./bin/kumabox clone golden --name demo-copy
```

The example runs without networking. Use `kumabox --help` and
`kumabox COMMAND --help` for command options. A
[systemd unit](packaging/systemd/kumabox.service) is included for service deployment;
`make guest` builds the native guest image using Docker.

## API and SDKs

The [Python and TypeScript SDKs](sdk/README.md) use the versioned HTTP API.
The [E2B adapter](internal/api/e2b/README.md) is experimental and implements a
subset of the protocol.

## License

[MIT](LICENSE)
