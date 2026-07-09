#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
root_dir="/tmp/kumabox-p0/data"
ref="ghcr.io/cocoonstack/cocoon/ubuntu:24.04"
platform="linux/amd64"
source="auto"

usage() {
  cat <<'USAGE'
Usage: scripts/linux/verify-oci-content-store.sh [options]

Options:
  --kumabox PATH    kumabox binary path, defaults to ./bin/kumabox
  --root-dir PATH   state root directory, defaults to /tmp/kumabox-p0/data
  --ref REF         OCI image ref, defaults to ghcr.io/cocoonstack/cocoon/ubuntu:24.04
  --platform VALUE  OCI platform, defaults to linux/amd64
  --source VALUE    OCI source: auto, registry, or daemon. Defaults to auto

Verifies P3-02 OCI content blob store:
pull-oci downloads manifest/config/layers into data/oci/content/blobs by digest,
writes content index, and hits cache on repeated pulls.
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
    --source)
      require_value "$1" "${2:-}"
      source="$2"
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
  echo "jq is required for content-store verification" >&2
  exit 1
fi

step "pull OCI blobs"
pull_json="$("$kumabox_path" \
  --root-dir "$root_dir" \
  image pull-oci "$ref" \
  --platform "$platform" \
  --source "$source" \
  --json)"
printf '%s\n' "$pull_json"

schema="$(printf '%s' "$pull_json" | jq -r '.schemaVersion')"
digest_ref="$(printf '%s' "$pull_json" | jq -r '.digestRef')"
resolved_source="$(printf '%s' "$pull_json" | jq -r '.source')"
manifest_path="$(printf '%s' "$pull_json" | jq -r '.manifest.path')"
config_path="$(printf '%s' "$pull_json" | jq -r '.config.path')"
layers_len="$(printf '%s' "$pull_json" | jq '.layers | length')"
downloaded="$(printf '%s' "$pull_json" | jq '.downloaded')"

if [[ "$schema" != "kumabox.oci.content.pull.v1" ]]; then
  echo "unexpected schemaVersion: $schema" >&2
  exit 1
fi
if [[ "$digest_ref" != *@sha256:* ]]; then
  echo "digestRef is not pinned: $digest_ref" >&2
  exit 1
fi
if [[ "$source" != "auto" && "$resolved_source" != "$source" ]]; then
  echo "source mismatch: got $resolved_source want $source" >&2
  exit 1
fi
if [[ "$layers_len" -lt 1 ]]; then
  echo "expected at least one layer, got $layers_len" >&2
  exit 1
fi
if [[ "$downloaded" -lt 1 ]]; then
  echo "expected at least one downloaded blob on first pull" >&2
  exit 1
fi

for path in "$manifest_path" "$config_path"; do
  if [[ ! -s "$path" ]]; then
    echo "blob path is missing or empty: $path" >&2
    exit 1
  fi
  printf 'state: blob=%s size=%s\n' "$path" "$(stat -c '%s' "$path")"
done

mapfile -t layer_paths < <(printf '%s' "$pull_json" | jq -r '.layers[].path')
for path in "${layer_paths[@]}"; do
  if [[ ! -s "$path" ]]; then
    echo "layer blob path is missing or empty: $path" >&2
    exit 1
  fi
  printf 'state: layer=%s size=%s\n' "$path" "$(stat -c '%s' "$path")"
done

index_path="$root_dir/oci/content/index.json"
if [[ ! -s "$index_path" ]]; then
  echo "content index is missing: $index_path" >&2
  exit 1
fi
printf 'state: index=%s\n' "$index_path"
printf 'state: source=%s\n' "$resolved_source"

step "repeat pull should hit cache"
cached_json="$("$kumabox_path" \
  --root-dir "$root_dir" \
  image pull-oci "$ref" \
  --platform "$platform" \
  --source "$source" \
  --json)"
printf '%s\n' "$cached_json"

cached="$(printf '%s' "$cached_json" | jq '.cached')"
cached_layers="$(printf '%s' "$cached_json" | jq '.layers | length')"
if [[ "$cached_layers" -ne "$layers_len" ]]; then
  echo "layer count changed across pulls: got $cached_layers want $layers_len" >&2
  exit 1
fi
if [[ "$cached" -lt "$((layers_len + 2))" ]]; then
  echo "expected manifest/config/layers to be cached on repeat pull, cached=$cached" >&2
  exit 1
fi

echo "P3-02 OCI content store verification passed"
