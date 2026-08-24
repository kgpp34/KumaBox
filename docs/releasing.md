# Releasing KumaBox

KumaBox releases have two coupled deliverables:

1. GitHub Release archives for Linux amd64 and arm64. Each archive contains
   `kumabox`, `kumabox-agent`, `kumabox-check`, `LICENSE`, and `README.md`.
2. A multi-architecture Ubuntu guest image at
   `ghcr.io/kgpp34/kumabox/ubuntu:24.04` containing the same release's guest
   agent, kernel, initramfs, and boot integration.

Do not publish only one deliverable. A host binary and guest image from
different protocol generations may boot but lose exec, identity, or clone
readiness functionality.

## Before tagging

1. Ensure CI passes on `develop`.
2. Run `test/release/install.sh`.
3. Run the full Linux/KVM suite against both metadata backends:

   ```bash
   GO_BIN="$(go env GOROOT)/bin/go"
   sudo test/e2e/e2e.sh \
     --go-bin "$GO_BIN" \
     --network cni:kumabox \
     --metadata-backend sqlite
   sudo test/e2e/e2e.sh \
     --go-bin "$GO_BIN" \
     --network cni:kumabox \
     --metadata-backend json
   ```

4. Confirm the release notes call out CLI, metadata, snapshot, and guest-agent
   compatibility changes.
5. Create and push a signed semantic-version tag:

   ```bash
   git tag -s v0.1.0 -m "KumaBox v0.1.0"
   git push origin v0.1.0
   ```

The `Release` workflow cross-compiles both host archives, validates their file
layout and checksums, builds both guest architectures, pushes the guest
manifest, and finally creates the GitHub Release with a checksummed bootstrap
installer.

## First GHCR publication

GitHub Container Registry packages are private by default. After the first
successful image push, open the package settings for `kumabox/ubuntu`, change
visibility to **Public**, and keep workflow access enabled for this repository.
This is a one-time repository-owner action.

Verify anonymous access before announcing the release:

```bash
docker logout ghcr.io 2>/dev/null || true
docker buildx imagetools inspect ghcr.io/kgpp34/kumabox/ubuntu:24.04-v0.1.0
```

## Clean-host acceptance

Use a disposable Ubuntu host with KVM and no KumaBox source checkout. Follow
the README Quick Start exactly. Acceptance requires:

- `kumabox-check --upgrade` succeeds twice without damaging existing CNI
  configuration;
- the release archive checksum is verified by `scripts/install.sh`;
- the public guest image imports without registry credentials;
- the complete E2E workflow passes with both JSON and SQLite metadata;
- `run`, `exec`, `console`, running snapshot, clone, and cleanup all work;
- `kumabox ps` is empty after cleanup and `kumabox gc` reports no leaked E2E
  resources.

The mutable `24.04` guest tag is for the latest compatible release. Automation
and reproducible environments should pin `24.04-vX.Y.Z` or the OCI digest.
