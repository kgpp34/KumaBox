# KumaBox P12 rewrite rules

## Project and baseline

- This remains the KumaBox project. P12 rewrites KumaBox's core logic; it does not create a new product or fork Cocoon.
- Preserve `pre-p12-rewrite-20260909`, which points to `08bc8d7545492da96b5c0fc984afc549b4497af1`. Do not move or delete the tag.
- Use Cocoon at `../cocoon@27ae1e0b2a65c9082c7a1b33c5245bfe43a4854d` to align lifecycle design, ordering, commit boundaries, and failure recovery. Do not copy its package structure or treat it as KumaBox source code.
- Read `docs/implementation/14-large-scale-agent-cross-node.md` and `docs/implementation/15-p12-core-rewrite-plan-and-progress.md` before planning P12 work.

## Mandatory feature approval gate

Before starting any major feature, present all four items to the project owner:

1. Logic: inputs, outputs, preconditions, state transitions, operation order, failures, cancellation, retry, crash recovery, idempotency, and Cocoon parity.
2. Design: domain boundaries, data ownership, transactions, commit points, locks/leases, external side effects, reconciliation, alternatives, and tradeoffs.
3. Code abstraction: core types and invariants, consumer-owned interfaces, concrete implementations, errors, configuration, and test seams.
4. Project layout: directories and files to add/remove, package responsibilities, import direction, forbidden dependencies, and final legacy deletion scope.

Do not create or modify Go, proto, SQL, test, script, generated, or other implementation code until the project owner explicitly approves that feature's four-part proposal. Reading, analysis, diagrams, design documents, and progress-ledger updates are allowed before approval.

Each P12 Epic, each create/start/snapshot/clone/restore/delete/reconcile vertical path, and every change to the domain model, schema, operation state machine, public API, package boundary, or concurrency model is a major feature. Overall plan approval does not approve individual features. Material deviation from an approved proposal invalidates the approval; stop coding and request approval again.
