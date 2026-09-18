<p align="center">
  <img src="assets/logo.png" alt="KumaBox logo" width="180">
</p>

# KumaBox

A microVM sandbox runtime for AI agents. One node runs one daemon
(`kumaboxd`) that owns all state, plus a thin client (`kumabox`) with a
Docker-like command line; sandboxes are Cloud Hypervisor microVMs booted from
OCI images, with CNI networking, cgroups, snapshots and clone.

The rewrite currently provides the `kumabox` CLI, the host doctor, container
image management, persistent sandbox creation, and recoverable Cloud Hypervisor
start/stop. Each command opens its metadata store, performs one operation, and
exits. The remaining sandbox lifecycle is tracked in
[docs/ROADMAP.md](docs/ROADMAP.md).

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
| `cli`, `cli/image`, `cli/sandbox`, `cli/doctor` | Command trees, argument parsing and presentation |
| `core` | Application services, operation ordering and concrete adapter assembly |
| `types` | Shared image and sandbox resource models and value objects |
| `images` | Image import, verification, boot selection and removal rules |
| `images/catalog` | Persist image identities, name bindings and layer references |
| `images/source` | Read Docker archives, OCI layouts/archives and registries |
| `images/erofs` | Convert source layers and extract boot candidates |
| `sandbox`, `sandbox/catalog` | Sandbox filesystem ownership and metadata persistence |
| `disk` | Prepare and remove sandbox-owned sparse ext4 COW disks |
| `vmm`, `vmm/cloudhypervisor` | VMM backend contract, launch/process facts, and the Cloud Hypervisor adapter |
| `cgroup` | Per-sandbox cgroup v2 preparation and reclamation |
| `metadata`, `metadata/sqlite` | Engine-neutral transactions and the SQLite implementation |
| `storage`, `lock/flock` | Managed filesystem operations and file locks |
| `errdefs`, `version` | Error classification and build information |

`core` owns application workflows that cross module boundaries and connects
their concrete adapters. CLI handlers use those services. Shared resource data
belongs to `types`; capability interfaces stay beside their consumers and are
not collected in `types`. Modules do not import `core` or `cli`. These dependency
directions are enforced by depguard in `.golangci.yml`.

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
`/boot/initrd.img*` files. A bootable KumaBox image also declares the OCI config
label `io.kumabox.boot.profile=overlay-v1`; older images without the label remain
importable and inspectable but will be rejected by `start`. Image conversion
requires `mkfs.erofs` 1.8 or newer; unit and integration tests use a stand-in and
run on macOS without root/KVM. The
[synthetic fixture](testdata/oci-layout/README.md) cannot boot a VM.

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
The inspect response reports the declaration as `boot.profile`; an empty value
means the source did not declare a boot contract. KumaBox never guesses a profile
from kernel or initrd filenames.
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

The reference Ubuntu guest image is built from
[`oci-images/ubuntu`](oci-images/ubuntu). Its independently implemented initramfs
script consumes only `kumabox.*` kernel parameters and virtio serials. It does
not expose or depend on another runtime's guest protocol.

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
is removed. A final manifest cannot be removed while a sandbox pins it.

The [Linux acceptance runbook](docs/runbooks/s2-oci.md) covers real conversion,
registry pull, cancellation, concurrency, and crash/retry behavior.

## Create a sandbox

`create` resolves an existing local image, reserves the sandbox name and exact
manifest digest, then creates a private sparse ext4 COW directly at
`Data/sandboxes/<ID>/cow.raw`. It does not start a VMM.

```bash
kumabox create IMAGE --name NAME \
  --cpus 2 --memory 1GiB --storage 10GiB
```

Successful text output is the full sandbox UUID. `--json` returns an indented
object containing the ID, name, manifest digest, `created` state, resource
shape, and creation time. Progress is written to stderr. `mkfs.ext4` from
e2fsprogs must be available on the host.

`Created` means the disk and metadata exist but the sandbox has never started;
`Stopped` is reserved for a sandbox whose VMM has exited after a start. The
metadata schema is version 2. Existing version 1 roots are migrated in one
transaction when first opened: image records and artifacts remain in place,
and the new sandbox collections become available without changing CLI roots.

`start SANDBOX` accepts an exact name or complete UUID. It checks KVM, the
Cloud Hypervisor executable, the pinned image, its declared `overlay-v1` boot
profile, and the existing ext4 COW before committing `Starting`. It records a
PID-reuse-safe process identity and commits `Running` only after the private
Cloud Hypervisor API reports readiness. Retrying recovers the same `Starting`
generation; a failed launch is terminated and retained as `Error` with a
diagnostic. `--json` returns the complete indented sandbox object.

`stop SANDBOX` follows the same direct-boot behavior as Cocoon. It first makes
a best-effort request to Cloud Hypervisor's private `vm.shutdown` endpoint,
then terminates the exact identity-checked VMM process with `SIGTERM`, waits up
to five seconds, and uses `SIGKILL` if it is still alive. There is no guest ACPI
shutdown wait and no `--force` or `--timeout` mode. Runtime files and the empty
cgroup are removed before the generation-fenced transition to `Stopped`.

```bash
kumabox stop NAME
kumabox stop 123e4567-e89b-42d3-a456-426614174000 --json
```

An interrupted stop retains `Stopping`; running the same command again resumes
the operation. It also recovers `Starting` records left by an interrupted start.
Stopping an already `Created` or `Stopped` sandbox succeeds without changing
its lifecycle history.

Attach to the direct-boot PTY of a running sandbox with `console`. The command
verifies the current process generation and Cloud Hypervisor API state before
opening the kernel PTY, switches the local terminal to raw mode, and restores it
on every exit path. Press `Ctrl-]` followed by `.` to detach without stopping
the sandbox; use `--escape-char` to select another ASCII escape character.

```bash
kumabox console NAME
kumabox console 123e4567-e89b-42d3-a456-426614174000 --escape-char '^A'
```

Console requires terminal stdin. A concurrent `stop` closes the PTY session;
the console command does not hold the sandbox operation lock while relaying I/O.

List active sandboxes with `ps`, or include created, stopped, failed, and
deleting records with `-a`:

```bash
kumabox ps
kumabox ps -a
kumabox ps -a --quiet
kumabox ps -a --json
```

The table always includes headers and prints complete sandbox UUIDs that can be
passed directly to `rm`. `--quiet` writes only those UUIDs, one per line. JSON
uses the same complete resource facts as `create --json` and returns `[]` for
an empty result.

Inspect one sandbox by its exact name or complete UUID. The command always
writes indented JSON, including retained failure diagnostics when present:

```bash
kumabox inspect NAME
kumabox inspect 123e4567-e89b-42d3-a456-426614174000
```

Remove a non-running sandbox by its exact name or complete UUID:

```bash
kumabox rm NAME
kumabox rm 123e4567-e89b-42d3-a456-426614174000 --json
```

Removal records durable `Deleting` intent before deleting the private disk.
If cleanup is interrupted, running the same command again resumes it. The
sandbox name and image reference are released together only after disk cleanup
succeeds. Text output is the removed sandbox's full UUID; `--json` returns its
ID and released name. Active lifecycle states are rejected until the sandbox
has been stopped with `kumabox stop`.

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
