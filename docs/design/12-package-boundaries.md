# Package boundaries

KumaBox is a daemonless CLI. Each command opens the same durable metadata,
constructs the required capabilities, performs one operation, and exits. The
package tree must therefore describe product concepts directly. It must not
look like an application-layer stack or expose persistence implementation
names as top-level concepts.

## Why the structure changed

The old `internal` tree placed different kinds of packages at the same level:

- `imagestore`, `imageimport`, `oci`, `ocibuild`, `ociresolver`, `ocisource`,
  and `ocistore` all participate in one image workflow;
- `vmstore` owns the durable VM model while `runtime` owns VM lifecycle;
- `storage` actually manages VM block disks, not all project storage;
- `lockfile` and `resourceguard` are two layers of one locking capability;
- `resources` constructs all durable capabilities while `state` describes
  interfaces for the same capabilities;
- `store`, `storage`, `resources`, and `state` do not tell a reader which data
  or behavior they own.

This made a reader follow imports before they could answer basic questions such
as "where is an image imported?" or "which package owns a VM record?".

## Current tree

```text
internal/
  agent/                 guest communication
  backend/               VMM adapters
  cli/                   argument parsing and output
  config/                host configuration
  disk/                  VM block-disk preparation and inspection
  image/                 managed image model and non-OCI import
    oci/                 OCI resolve, fetch, build, and import pipeline
  lock/                  daemonless process and resource locks
  meta/                  typed metadata engine
    json/                JSON-file backend
    sqlite/              SQLite backend
  network/               network model, CNI/TAP lifecycle, reconciliation
  snapshot/              snapshot model and artifact lifecycle
  state/                 process-level capability assembly only
  vm/                    VM model and durable state transitions
    nocloud/             VM NoCloud metadata generation
    runtime/             VM lifecycle orchestration
```

Small supporting packages such as `version`, `fault`, and `fileutil` may stay
at the top level when they have one precise meaning. A package is not moved
merely to reduce the package count.

## Ownership rules

### `meta`

`meta` owns the generic typed persistence contract, transactions, migration,
backup, and the JSON/SQLite implementations. It does not know what starting a
VM or importing an image means.

The word `Store` may still be used for a concrete type inside this package, but
metadata implementation names must not become product-level package names.

### `vm`

`vm` owns VM records, desired and observed state, state transitions, attached
device descriptions, and VM-specific persistence operations. Callers should
read `vm.Record`, not `vmstore.VMRecord`.

`vm/runtime` coordinates backend, disk, network, snapshot, and agent actions.
It does not own their durable data formats.

### `disk`

`disk` owns block-disk files and tools such as `qemu-img` and filesystem
creation. It replaces the ambiguous top-level name `storage`; metadata
storage remains in `meta`.

### `image`

`image` owns managed image records, local/cloud image import, validation, and
catalog operations. OCI is an image source and build pipeline, so all OCI
packages live under `image/oci` rather than appearing as unrelated top-level
subsystems.

### `snapshot`

`snapshot` owns snapshot records, captured artifacts, export/import, and
snapshot dependency rules. VM restore orchestration remains in `vm/runtime`.

### `network`

`network` owns network records, CNI/TAP allocation, journals, cleanup, and
reconciliation. It does not depend on VM lifecycle orchestration.

### `lock`

`lock` owns both low-level advisory file locks and the higher-level mutation,
maintenance, and per-resource lock policy. Callers should not need to know
that coordination is implemented with lock files.

### `state`

`state` is the only process-level assembly boundary. It opens the configured
metadata engine and exposes the VM, image, snapshot, network, operation,
reference, and metering capabilities required by one CLI invocation.

It does not define a second business model and is not named `resources`
or `StoreSet`. The call site is `state.Open(cfg)`, returning a
`state.Set` whose fields use product terms rather than persistence suffixes.

## Completed migration

| Former path | Current path | Reason |
| --- | --- | --- |
| `internal/metastore` | `internal/meta` | Metadata is the concept; store is an implementation detail. |
| `internal/lockfile` + `internal/resourceguard` | `internal/lock` | One coordination capability with two levels. |
| `internal/vmstore` | `internal/vm` | The package owns the VM model, not merely a store. |
| `internal/runtime` | `internal/vm/runtime` | Lifecycle orchestration belongs under VM. |
| `internal/storage` | `internal/disk` | The code manages VM block disks. |
| `internal/imagestore` + `internal/imageimport` | `internal/image` | Catalog and import are one image capability. |
| `internal/oci*` | `internal/image/oci` | Resolve, source, content, build, and import form one pipeline. |
| `internal/resources` + `internal/state` | `internal/state` | Keep one assembly boundary and one vocabulary. |

## Dependency direction

```text
cli
  -> state
  -> vm/runtime

vm/runtime
  -> vm, image, disk, network, snapshot, agent, backend, lock

state
  -> meta
  -> vm, image, network, snapshot and other durable capabilities

domain packages
  -> meta
```

Domain packages must not import `cli`, `state`, or `vm/runtime`. `state` may
construct concrete packages, but it must not contain lifecycle behavior. This
keeps the daemonless construction path explicit without introducing DDD-style
application or repository layers.

## Enforced rules

- No top-level `*store` package exists under `internal`.
- No top-level `oci*` package exists; OCI code is discoverable under image.
- `storage` is not used to mean both metadata persistence and VM disks.
- `state` is the single process-level assembly abstraction.
- Package names match directory names and are singular.
- JSON and SQLite expose the same domain behavior through `meta`.
- Structural changes must preserve unit tests, race tests, vet, formatting,
  and lint.
