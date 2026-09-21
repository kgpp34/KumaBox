# Releasing KumaBox

A release requires both repository gates and Linux acceptance evidence.

## 1. Repository gates

```bash
make verify
make race
make lint
```

The worktree must contain no generated binary, coverage output, or unrelated artifact. Documentation links must pass `make docs-check`.

## 2. Linux acceptance

Run the applicable checked-in runbooks on a Linux/KVM host. Record:

- commit and version;
- kernel and distribution;
- Cloud Hypervisor, `mkfs.erofs`, `mkfs.ext4`, and Go versions;
- CPU architecture and cgroup mode;
- exact commands, exit codes, image digests, and any retained cleanup state.

At minimum, a lifecycle release must cover image import/verify, create/start/exec/console/stop/restart/rm, cancellation, and host process identity checks. Network releases must also cover CNI ADD/DEL, outbound connectivity, quiesce/recover, and partial-failure retry.

## 3. Build

```bash
make clean
make build
make agent
```

`bin/kumabox` is the host CLI, `bin/kumabox-check` is the host checker, and `bin/kumabox-agent` is the Linux guest agent.

## 4. Version

Build metadata is injected through `version.Version`, `version.Commit`, and `version.BuildTime`. Create a signed or annotated version tag only after the release commit and Linux evidence are final.
