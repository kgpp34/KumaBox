# KumaBox working rules

## Project and baseline

- This remains the KumaBox project. The core logic is being rewritten; it does not create a second product and does not fork Cocoon.
- Preserve the tag `pre-p12-rewrite-20260909` (`08bc8d7545492da96b5c0fc984afc549b4497af1`). Never move or delete it. The pre-rewrite tree is a **read-only reference**: read it to confirm behaviour, ordering and failure semantics; never copy its code, types, schema or tests.
- Cocoon at `../cocoon@27ae1e0b2a65c9082c7a1b33c5245bfe43a4854d` is the capability floor and behavioural reference. Match its lifecycle ordering, commit boundaries and failure recovery; do not copy its package structure, god packages, hook-bag control flow or dual metadata backends.
- Read these before doing any work, in this order: `docs/PRODUCT.md`, `docs/ARCHITECTURE.md`, `docs/BEHAVIOR.md`, `docs/HOST.md`, `docs/PERFORMANCE.md`, `docs/ROADMAP.md`, `docs/DECISIONS.md`.
- The design documents previously numbered 14–19 under `docs/implementation/` were written by ChatGPT/Codex, are deleted, and must not be recreated or referenced (DEC-011). `docs/implementation/00`–`13` are history only, never a specification.

## Hard rules

1. **Zero code reuse.** Every implementation is written from the specifications. The old tree is reference material only (DEC-003).
2. **Layer direction is enforced by tooling, not by a bespoke test suite.** `ARCHITECTURE.md` §12.1 maps each rule to a standard linter (`depguard` for the layer matrix, `gochecknoinits`, `forbidigo`, `revive`) configured in `.golangci.yml`, which lands with the first real packages in S1. Do not add a hand-written architecture test package: rules live in the spec, enforcement lives in linter config.
3. **Naming and style follow `ARCHITECTURE.md` §13.** No `utils`/`helpers`/`common`/`Manager`/`Service`/`Impl` naming, no `init()`, no package-level mutable state, no `_ =` error discard.
4. **Tests must pass locally before a phase is called done.** `make verify` and `make race` must be green on macOS with no root and no KVM. Anything needing a real Cloud Hypervisor, CNI or KVM host is verified manually by the project owner, so every phase must ship a copy-pasteable runbook with expected output (DEC-009).
5. **Performance claims need the protocol.** Use `PERFORMANCE.md` §2: same machine, same parameters, N ≥ 30, P50/P95/P99, raw data archived, differences judged with `benchstat`, and every claimed win traceable to one code mechanism (DEC-008).
6. **Do not weaken safety for speed.** Never trade away digests, path boundaries, references, leases, dirty markers or durability.

## Approval gate

A **phase** from `ROADMAP.md` (S1 image vertical, S2 sandbox vertical, S3 network, S4 snapshot/restore, S5 clone, S6 convergence, S7 cross-node, S8 production gates) is the unit of approval. The current phase proposal lives in `docs/proposals/`. Before writing implementation code for a phase, present all four items to the project owner and get an explicit answer:

1. **Logic** — what the phase makes work end to end; inputs, outputs, preconditions, state transitions, step order, cancellation, retry, crash recovery, idempotency, and the Cocoon behaviour it matches or deliberately improves.
2. **Design** — which module owns which durable fact, transaction and commit boundaries, locks and leases, external side effects, reconciliation, alternatives considered and why they were rejected.
3. **Code abstraction** — core types and invariants, the consumer-owned ports (1–3 methods, each with an in-package fake), concrete implementations, errors, configuration, and test seams.
4. **Layout** — directories and files added or removed, package responsibilities, import direction, forbidden dependencies, and what gets deleted at the end of the phase.

Reading, analysis, diagrams, design documents and roadmap updates are allowed before approval. Do not create or modify Go, proto, SQL, test, script or generated code until the phase is approved.

Scope changes invalidate the approval: if implementation needs a different domain model, schema, operation state machine, public API, package boundary or concurrency model than the approved proposal, stop and ask again.
