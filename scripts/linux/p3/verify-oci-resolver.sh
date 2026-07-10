#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
root_dir="/tmp/kumabox-p0/data"
name="p3-resolve"
ref="ghcr.io/cocoonstack/cocoon/ubuntu:24.04"
platform="linux/amd64"

usage() {
  cat <<'USAGE'
Usage: scripts/linux/p3/verify-oci-resolver.sh [options]

Options:
  --kumabox PATH    kumabox binary path, defaults to ./bin/kumabox
  --root-dir PATH   state root directory, defaults to /tmp/kumabox-p0/data
  --name NAME       dry-run image name, defaults to p3-resolve
  --ref REF         OCI image ref, defaults to ghcr.io/cocoonstack/cocoon/ubuntu:24.04
  --platform VALUE  OCI platform, defaults to linux/amd64

Verifies P3-01 OCI ref resolver:
image build REF --dry-run --json resolves tag -> digest and layer descriptors
without publishing an image into the image store.
USAGE
}

require_value() {
  local flag="$1"
  local value="${2:-}"
  if [[ -z "$value" ]]; then
    echo "$flag requires a value" >&2
    exit 2
  fi
}

step() {
  printf '\n==> %s\n' "$1"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --kumabox)
      require_value "$1" "${2:-}"
      kumabox_path="$2"
      shift 2
      ;;
    --root-dir)
      require_value "$1" "${2:-}"
      root_dir="$2"
      shift 2
      ;;
    --name)
      require_value "$1" "${2:-}"
      name="$2"
      shift 2
      ;;
    --ref)
      require_value "$1" "${2:-}"
      ref="$2"
      shift 2
      ;;
    --platform)
      require_value "$1" "${2:-}"
      platform="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ ! -x "$kumabox_path" ]]; then
  echo "kumabox is not executable: $kumabox_path" >&2
  exit 1
fi

if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required for resolver verification" >&2
  exit 1
fi

step "resolve OCI ref"
resolve_json="$("$kumabox_path" \
  --root-dir "$root_dir" \
  image build "$ref" \
  --name "$name" \
  --platform "$platform" \
  --dry-run \
  --json)"
printf '%s\n' "$resolve_json"

schema="$(printf '%s' "$resolve_json" | jq -r '.schemaVersion')"
digest="$(printf '%s' "$resolve_json" | jq -r '.result.resolvedDigest')"
digest_ref="$(printf '%s' "$resolve_json" | jq -r '.result.digestRef')"
layers_len="$(printf '%s' "$resolve_json" | jq '.result.layers | length')"
config_digest="$(printf '%s' "$resolve_json" | jq -r '.result.config.digest')"
resolved_platform="$(printf '%s' "$resolve_json" | jq -r '.result.platform.os + "/" + .result.platform.architecture')"

if [[ "$schema" != "kumabox.oci.resolve.v1" ]]; then
  echo "unexpected schemaVersion: $schema" >&2
  exit 1
fi
if [[ "$digest" != sha256:* ]]; then
  echo "resolved digest is not sha256: $digest" >&2
  exit 1
fi
if [[ "$digest_ref" != *@sha256:* ]]; then
  echo "digestRef is not pinned: $digest_ref" >&2
  exit 1
fi
if [[ "$config_digest" != sha256:* ]]; then
  echo "config digest is not sha256: $config_digest" >&2
  exit 1
fi
if [[ "$layers_len" -lt 1 ]]; then
  echo "expected at least one layer, got $layers_len" >&2
  exit 1
fi
if [[ "$resolved_platform" != "$platform" ]]; then
  echo "platform mismatch: got $resolved_platform want $platform" >&2
  exit 1
fi

step "resolver summary"
printf 'state: ref=%s\n' "$ref"
printf 'state: digest=%s\n' "$digest"
printf 'state: digestRef=%s\n' "$digest_ref"
printf 'state: config=%s\n' "$config_digest"
printf 'state: layers=%s\n' "$layers_len"

step "assert dry-run did not publish image"
if "$kumabox_path" --root-dir "$root_dir" image inspect "$name" --json >/tmp/kumabox-p3-resolve-inspect.json 2>/tmp/kumabox-p3-resolve-inspect.err; then
  echo "dry-run unexpectedly published image $name" >&2
  cat /tmp/kumabox-p3-resolve-inspect.json >&2
  exit 1
fi
cat /tmp/kumabox-p3-resolve-inspect.err

echo "P3-01 OCI ref resolver verification passed"
