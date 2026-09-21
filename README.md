<p align="center">
  <img src="assets/logo.png" alt="KumaBox logo" width="180">
</p>

# KumaBox

KumaBox is a daemonless microVM sandbox runtime for AI agents. The current implementation imports OCI and Docker images, creates persistent sandboxes, boots them with Cloud Hypervisor, and provides console and guest command access.

KumaBox uses Cocoon commit `27ae1e0b2a65c9082c7a1b33c5245bfe43a4854d` as its behavior and capability baseline. Compatibility is evaluated feature by feature. KumaBox keeps its own package structure, error model, metadata schema, and `kumabox.*` guest boot protocol.

## Current status

Available commands:

```text
kumabox doctor
kumabox image pull|import|list|inspect|verify|remove
kumabox create|start|stop|ps|inspect|logs|console|exec|rm
kumabox version
```

Image import and sandbox lifecycle are implemented locally. Real Cloud Hypervisor, cgroup, vsock, ext4, and EROFS behavior requires Linux and is covered by the checked-in runbooks. Networking, `run`, snapshots, clone, and Firecracker remain planned work; see [the roadmap](docs/ROADMAP.md).

## Build and test

KumaBox requires Go 1.24 or newer.

```bash
make build
make verify
make lint
```

The binaries are written to `bin/`. `make verify` checks formatting, documentation links, Linux and Darwin vet, shell syntax, race-enabled tests, and the build.

Install locally with:

```bash
sudo make install
kumabox doctor
```

The host checker reports Linux, KVM, cgroup v2, Cloud Hypervisor, `mkfs.erofs`, `mkfs.ext4`, and other runtime prerequisites. See [host requirements](docs/HOST.md).

## Image workflow

A bootable image must contain a kernel and initramfs and declare the OCI label `io.kumabox.boot.profile=overlay-v1`.

```bash
kumabox image pull ghcr.io/example/image:tag --platform linux/amd64
kumabox image import demo ./docker-save.tar --format docker --platform linux/amd64
kumabox image import demo ./oci-layout --format oci --platform linux/amd64
kumabox image list
kumabox image inspect demo
kumabox image verify demo
```

Local import auto-detects OCI layouts, OCI archives, and `docker save` archives. `docker export` filesystem archives are unsupported. Source layers are verified, converted to EROFS, and published by digest. Boot candidates follow layer overwrite, whiteout, and opaque-directory semantics.

## Sandbox workflow

```bash
kumabox create demo --name box --cpus 2 --memory 1GiB --storage 10GiB
kumabox start box
kumabox exec box -- uname -a
kumabox logs --tail 50 box
kumabox logs -f box
kumabox console box
kumabox stop box
kumabox rm box
```

`create` prepares a sparse ext4 COW disk but does not start the VMM. `start` uses direct kernel boot, records a PID-reuse-safe process identity, and commits `running` only after the Cloud Hypervisor API reports readiness. `stop` requests shutdown, then uses an identity-checked TERM-to-KILL fallback. `exec` uses the guest agent over private hybrid-vsock transport. `logs` reads persistent backend output with tail and follow support, including after stop.

`ps` prints a table with headers. `inspect` and every `--json` mode emit indented JSON. Progress goes to stderr; command results go to stdout.

## Configuration

Configuration precedence is:

```text
explicit flag > environment > explicit --config file > default
```

KumaBox never searches for an implicit configuration file. Root paths can be set with `--root-dir`, `--run-dir`, and `--log-dir`. All settings and environment variable names are documented in [configuration](docs/CONFIGURATION.md).

## Architecture

The repository uses root-level modules instead of `internal` or a generic `pkg` tree:

| Package | Responsibility |
|---|---|
| `cli` | Cobra command tree, argument validation, and presentation |
| `core` | Application services and concrete adapter assembly |
| `types` | Shared image and sandbox values; no capability interfaces |
| `images` | Source resolution, conversion, verification, and removal |
| `sandbox`, `disk` | Sandbox paths, locks, and writable COW disks |
| `vmm` | VMM contracts, launch plans, process identity, and backend registry |
| `vmm/cloudhypervisor` | Cloud Hypervisor process and API adapter |
| `agent` | Guest exec protocol and host/guest transports |
| `metadata` | Transaction contracts and SQLite implementation |
| `cgroup`, `storage`, `lock/flock` | Host resource adapters |

The full ownership and dependency rules are in [architecture](docs/ARCHITECTURE.md). User-visible contracts are in [behavior](docs/BEHAVIOR.md).

## Documentation

Start with [docs/README.md](docs/README.md). Normative design documents, accepted decisions, runbooks, and the active roadmap are tracked in Git and reviewed with code.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Changes to command behavior, architecture, persistent data, or guest protocols must update the corresponding normative document in the same commit.

## License

[MIT](LICENSE)
