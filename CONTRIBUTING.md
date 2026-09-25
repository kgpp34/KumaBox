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

Run the relevant local Linux acceptance procedure for changes involving KVM, Cloud Hypervisor, cgroup v2, CNI, EROFS, ext4, or vsock. Record the host versions and result in the pull request.

## Compatibility

Cocoon commit `27ae1e0b2a65c9082c7a1b33c5245bfe43a4854d` is the feature and behavior reference. A change may intentionally differ when KumaBox has a stronger safety or modularity guarantee; explain material behavior differences in the pull request. Design notes under `docs/` are local working material and must not be committed.
