<p align="center">
  <img src="assets/logo.png" alt="KumaBox logo" width="180">
</p>

# KumaBox

KumaBox is being rebuilt as a modular infrastructure project for high-density
agent sandboxes.

The rewrite follows Cocoon's core lifecycle semantics—such as create, snapshot,
clone, and restore—while keeping KumaBox's architecture and implementation
independent. The current branch is not a usable release until those capabilities
are reintroduced through the approved P12 milestones.

## Rewrite rules

- Infrastructure capability modules are the primary architectural boundary.
- Lifecycle behavior is specified and compared with Cocoon before implementation.
- Each major capability requires an approved logic, design, abstraction, and
  directory proposal before code is written.
- Tests and architecture checks are delivered with each capability.

Local design and progress records live under `docs/` and are intentionally not
tracked by Git.

## Development

```bash
make verify
```

## Recovery

The pre-rewrite source is recoverable from the protected Git tag
`pre-p12-rewrite-20260909`. A verified local source archive is also stored under
`.rewrite-backup/` in the rewrite workspace.

## License

[MIT](LICENSE)
