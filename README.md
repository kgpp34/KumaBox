<p align="center">
  <img src="assets/logo.png" alt="KumaBox logo" width="180">
</p>

# KumaBox

A microVM sandbox runtime for AI agents. One node runs one daemon
(`kumaboxd`) that owns all state, plus a thin client (`kumabox`) with a
Docker-like command line; sandboxes are Cloud Hypervisor microVMs booted from
OCI images, with CNI networking, cgroups, snapshots and clone.

The rewrite currently provides the `kumabox` CLI, the host doctor, and container
image management: registry pull, Docker/OCI import, list, inspect, verify,
and remove. Each command opens its metadata store and exits. VM lifecycle and
a daemon are later phases of [docs/ROADMAP.md](docs/ROADMAP.md).

## Where the design lives

Read these in order. They are the only specifications; anything else under
`docs/` is history.

| Document | Answers |
|---|---|
| [docs/PRODUCT.md](docs/PRODUCT.md) | What this is, who uses it, what v1 must do, what it will not do, how it relates to Cocoon, shared vocabulary |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Layering and the import matrix, fact ownership, the execution model, transactions and locks, cross-cutting contracts, testing tiers, naming and code style |
| [docs/BEHAVIOR.md](docs/BEHAVIOR.md) | What happens on the machine when a command runs, and what is left behind when it fails |
| [docs/PERFORMANCE.md](docs/PERFORMANCE.md) | How performance is measured, how it is compared against Cocoon, and which scenarios must match or beat it |
| [docs/ROADMAP.md](docs/ROADMAP.md) | What each phase does, why, how, and the evidence that closes it |
| [docs/DECISIONS.md](docs/DECISIONS.md) | Decisions taken, why the previous design was discarded, what is still open |

Documentation is intentionally not tracked by Git (see `.gitignore`), so these
files live only in the working tree — keep local backups.

## Working on it

```bash
make verify   # formatting, vet, tests, build
make race     # race detector, required for concurrency changes
```

`make verify` must stay green on macOS with no root and no KVM. Real microVM
behaviour (Cloud Hypervisor, CNI, KVM) is verified manually on a Linux host
using the runbook attached to each phase.

Code is organized as importable modules by responsibility, without `internal`
or a generic `pkg` container:

| Package | Responsibility |
|---|---|
| `cmd/kumabox` | Process entry point, signals and exit status |
| `cli`, `cli/image`, `cli/doctor` | Command trees, argument parsing and presentation |
| `core` | Assemble concrete adapters and own their resources |
| `images` | Managed image model, import, verification and removal rules |
| `images/catalog` | Persist image identities, name bindings and layer references |
| `images/source` | Read Docker archives, OCI layouts/archives and registries |
| `images/erofs` | Convert source layers and extract boot candidates |
| `metadata`, `metadata/sqlite` | Engine-neutral transactions and the SQLite implementation |
| `storage`, `lock/flock` | Managed filesystem operations and file locks |
| `errdefs`, `version` | Error classification and build information |

`core` connects modules through constructors; it is not a second implementation
of their business operations. CLI handlers use the assembled modules. Only
`core` selects concrete image and metadata adapters. The image core does not
import adapters or metadata engines, and modules do not import `core` or `cli`.
These dependency directions are enforced by depguard in `.golangci.yml`.

The image command groups complete responsibilities into `import.go` (pull and
local import), `query.go` (list, inspect and verify), and `remove.go`. Related
types, interfaces and methods stay together; files are not split by declaration
kind. Interfaces describe the operations needed by their consumers.

Document each package's responsibility in an existing source file. Exported APIs,
key types and fields, and complex private methods need comments explaining their
contracts, units, ownership, and failure boundaries. Keep comments in English and
use indented ASCII diagrams near workflows where ordering, locking, or commit
boundaries matter. Update these comments whenever the behavior changes.

Tests live in their owning directories as `*_test.go`. The shared memory/SQLite
transaction contract is exercised in `metadata/store_test.go`; there is no
production package for test helpers. Image workflow integration tests use the
public module APIs and cover the assembled catalog with both metadata engines.

Build with `make build`, or run the entry point with `go run ./cmd/kumabox`.
The host checker source is `scripts/kumabox-check.sh`.

## Container images

Pull or import a Linux image containing regular `/boot/vmlinuz*` and
`/boot/initrd.img*` files. Image conversion requires `mkfs.erofs` 1.8 or newer;
unit and integration tests use a stand-in and run on macOS without root/KVM.
The [synthetic fixture](testdata/oci-layout/README.md) cannot boot a VM.

```bash
kumabox image pull REGISTRY/IMAGE:TAG --platform linux/amd64
kumabox image import tiny ./testdata/oci-layout --platform linux/amd64
kumabox image import demo ./docker-save.tar --format docker --platform linux/amd64
kumabox image ls --json
kumabox image inspect tiny
kumabox image verify tiny
kumabox image rm tiny
```

`image ls` prints a table with names, 12-character image IDs, platforms,
human-readable sizes, and creation timestamps in UTC. `image inspect` and
`image ls --json` print indented JSON with full digests and numeric sizes.
Import and pull show a live spinner and completed layer counts on a terminal.
Verification and removal also show waiting status; removal reports completed
image counts. Redirected progress uses plain lines on stderr. Results are
written to stdout.

`image import NAME PATH` detects the format from source contents by default.
It accepts OCI layout directories, OCI archives, and `docker save` archives;
archives can be plain tar or gzip, regardless of their filename extension.
Use `--format oci` or `--format docker` to select a format explicitly.
For Docker archives containing multiple images for the target platform, use
`--source-tag REPOSITORY:TAG` to select the source image; `NAME` is its local
KumaBox name. Archives containing both OCI and Docker metadata use OCI by
default; pass `--format docker --source-tag REPOSITORY:TAG` for Docker tag
selection. `docker export` filesystem archives are not supported.

To import an image already present in Docker:

```bash
docker save -o demo.tar your-image:tag
kumabox image import demo ./demo.tar --platform linux/amd64
kumabox image verify demo
```

Docker archives are normalized to a deterministic OCI manifest. Its digest
identifies the imported config and ordered layers and may differ from the
original registry manifest digest. Repacking or changing source tags preserves
the imported digest. Docker images must meet the same kernel/initrd requirements.

For a separate data store, pass all three roots:

```bash
kumabox --root-dir /tmp/kb/data --run-dir /tmp/kb/run --log-dir /tmp/kb/log image ls --json
```

Import validates OCI manifest/config/layer digests and layer diffIDs, streams
layers to EROFS, and extracts boot candidates with layer overwrite/whiteout
semantics. Metadata is committed after durable publication and final digest
checks. Verification detects EROFS and boot-file corruption. Source OCI blobs
are never stored persistently; repeated imports reuse verified, registered
artifacts. Removing a name retains artifacts until the last image reference
is removed.

The [Linux acceptance runbook](docs/runbooks/s2-oci.md) covers real conversion,
registry pull, cancellation, concurrency, and crash/retry behavior.

## Reference material

- Cocoon at `../cocoon@27ae1e0b2a65c9082c7a1b33c5245bfe43a4854d` is the
  capability floor: match its lifecycle ordering and failure recovery, never
  copy its package structure or its dual metadata backends.
- The pre-rewrite KumaBox source is available read-only from the protected tag
  `pre-p12-rewrite-20260909` (and as an archive under `.rewrite-backup/`). It is
  reference material for behaviour only; no code, types, schema or tests are
  reused from it.

## License

[MIT](LICENSE)
