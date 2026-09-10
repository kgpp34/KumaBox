<p align="center">
  <img src="assets/logo.png" alt="KumaBox logo" width="180">
</p>

# KumaBox

A microVM sandbox runtime for AI agents. One node runs one daemon
(`kumaboxd`) that owns all state, plus a thin client (`kumabox`) with a
Docker-like command line; sandboxes are Cloud Hypervisor microVMs booted from
OCI images, with CNI networking, cgroups, snapshots and clone.

The core logic is being rewritten from scratch. **The current branch has no
product code yet** — it contains the specifications and the architecture gate
only. That is deliberate: each phase of `docs/ROADMAP.md` starts with a
four-part proposal, and no implementation code is written before it is
approved.

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
