# Contributing to KumaBox

## Prerequisites

- Go 1.24 or newer
- Bash
- Linux for real microVM acceptance tests
- `golangci-lint`, `gofumpt`, and `goimports` are installed into `bin/` by the Makefile when needed

## Local workflow

```bash
git clone https://github.com/kumabox/kumabox.git
cd kumabox
make verify
make lint
```

Keep changes within the owning module. KumaBox does not use `internal`, a generic `pkg`, or packages split only by declaration kind. Shared data belongs in `types`; capability interfaces stay with the module that owns or consumes the capability.

Before submitting a change:

```bash
make fmt
make verify
make lint
```

Run the relevant Linux acceptance runbook for changes involving KVM, Cloud Hypervisor, cgroup v2, CNI, EROFS, ext4, or vsock. Record the host versions and result in the pull request.

## Compatibility and documentation

Cocoon commit `27ae1e0b2a65c9082c7a1b33c5245bfe43a4854d` is the feature and behavior reference. A change may intentionally differ when KumaBox has a stronger safety or modularity guarantee, but the difference must be documented in [docs/COCOON-MAP.md](docs/COCOON-MAP.md).

Update documentation in the same commit when a change affects:

- commands, flags, output, exit codes, or lifecycle behavior;
- package ownership or dependency direction;
- persisted metadata, managed paths, or recovery rules;
- host requirements, configuration, or guest protocols;
- roadmap status or Linux acceptance steps.

Use `make docs-check` to validate relative Markdown links.
