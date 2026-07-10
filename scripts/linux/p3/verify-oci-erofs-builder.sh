#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
root_dir="/tmp/kumabox-p0/data"
ref="kumabox/ubuntu:24.04-p3"
platform="linux/amd64"
name="p3-erofs"
source="auto"
mkfs_erofs="mkfs.erofs"

usage() {
  cat <<'USAGE'
Usage: scripts/linux/p3/verify-oci-erofs-builder.sh [options]

Options:
  --kumabox PATH      kumabox binary path, defaults to ./bin/kumabox
  --root-dir PATH     state root directory, defaults to /tmp/kumabox-p0/data
  --ref REF           OCI image ref, defaults to kumabox/ubuntu:24.04-p3
  --platform VALUE    OCI platform, defaults to linux/amd64
  --name NAME         image name, defaults to p3-erofs
  --source VALUE      OCI source: auto, registry, or daemon. Defaults to auto
  --mkfs-erofs PATH   mkfs.erofs binary path, defaults to mkfs.erofs

Verifies OCI layer -> EROFS conversion:
image build pulls OCI content, converts layers into shared .erofs blobs, records
layer order in image metadata, and reuses the same EROFS blobs on a repeat build.
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
    --name)
      require_value "$1" "${2:-}"
      name="$2"
      shift 2
      ;;
    --source)
      require_value "$1" "${2:-}"
      source="$2"
      shift 2
      ;;
    --mkfs-erofs)
      require_value "$1" "${2:-}"
      mkfs_erofs="$2"
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
  echo "jq is required for EROFS builder verification" >&2
  exit 1
fi
if [[ "$mkfs_erofs" == */* ]]; then
  if [[ ! -x "$mkfs_erofs" ]]; then
    echo "mkfs.erofs is not executable: $mkfs_erofs" >&2
    exit 1
  fi
elif ! command -v "$mkfs_erofs" >/dev/null 2>&1; then
  echo "mkfs.erofs is required for EROFS builder verification" >&2
  exit 1
fi

cleanup_name="${name}-cached"

step "clean previous EROFS image records"
"$kumabox_path" --root-dir "$root_dir" image rm "$name" >/dev/null 2>&1 || true
"$kumabox_path" --root-dir "$root_dir" image rm "$cleanup_name" >/dev/null 2>&1 || true

step "build OCI image layers into EROFS"
build_json="$("$kumabox_path" \
  --root-dir "$root_dir" \
  image build "$ref" \
  --name "$name" \
  --platform "$platform" \
  --source "$source" \
  --mkfs-erofs "$mkfs_erofs" \
  --json)"
printf '%s\n' "$build_json"

schema="$(printf '%s' "$build_json" | jq -r '.schemaVersion')"
image_id="$(printf '%s' "$build_json" | jq -r '.id')"
image_name="$(printf '%s' "$build_json" | jq -r '.name')"
oci_source="$(printf '%s' "$build_json" | jq -r '.oci.source')"
digest_ref="$(printf '%s' "$build_json" | jq -r '.oci.digestRef')"
layers_len="$(printf '%s' "$build_json" | jq '.oci.layers | length')"

if [[ "$schema" != "kumabox.image.v1" ]]; then
  echo "unexpected image schemaVersion: $schema" >&2
  exit 1
fi
if [[ "$image_name" != "$name" ]]; then
  echo "unexpected image name: $image_name" >&2
  exit 1
fi
if [[ "$digest_ref" != *@sha256:* ]]; then
  echo "OCI digestRef is not pinned: $digest_ref" >&2
  exit 1
fi
if [[ "$source" != "auto" && "$oci_source" != "$source" ]]; then
  echo "source mismatch: got $oci_source want $source" >&2
  exit 1
fi
if [[ "$layers_len" -lt 1 ]]; then
  echo "expected at least one OCI layer" >&2
  exit 1
fi
printf 'state: image=%s source=%s digestRef=%s layers=%s\n' "$name" "$oci_source" "$digest_ref" "$layers_len"

manifest_path="$root_dir/cloudimg/$image_id/image.json"
if [[ ! -s "$manifest_path" ]]; then
  echo "image manifest is missing: $manifest_path" >&2
  exit 1
fi
printf 'state: manifest=%s\n' "$manifest_path"

step "verify EROFS layer records"
for ((i = 0; i < layers_len; i++)); do
  layer_index="$(printf '%s' "$build_json" | jq -r ".oci.layers[$i].index")"
  layer_digest="$(printf '%s' "$build_json" | jq -r ".oci.layers[$i].digest")"
  erofs_path="$(printf '%s' "$build_json" | jq -r ".oci.layers[$i].erofs.path")"
  erofs_fs="$(printf '%s' "$build_json" | jq -r ".oci.layers[$i].erofs.filesystem")"
  erofs_source="$(printf '%s' "$build_json" | jq -r ".oci.layers[$i].erofs.sourceLayer")"

  if [[ "$layer_index" -ne "$i" ]]; then
    echo "layer index mismatch at $i: got $layer_index" >&2
    exit 1
  fi
  if [[ "$erofs_fs" != "erofs" ]]; then
    echo "layer $i filesystem is not erofs: $erofs_fs" >&2
    exit 1
  fi
  if [[ "$erofs_source" != "$layer_digest" ]]; then
    echo "layer $i source mismatch: got $erofs_source want $layer_digest" >&2
    exit 1
  fi
  if [[ ! -s "$erofs_path" ]]; then
    echo "EROFS blob missing or empty: $erofs_path" >&2
    exit 1
  fi
  printf 'state: layer[%s]=%s erofs=%s size=%s\n' "$i" "$layer_digest" "$erofs_path" "$(stat -c '%s' "$erofs_path")"
done

step "inspect image manifest"
inspect_json="$("$kumabox_path" --root-dir "$root_dir" image inspect "$name" --json)"
printf '%s\n' "$inspect_json"
inspect_digest_ref="$(printf '%s' "$inspect_json" | jq -r '.oci.digestRef')"
if [[ "$inspect_digest_ref" != "$digest_ref" ]]; then
  echo "inspect digestRef mismatch: got $inspect_digest_ref want $digest_ref" >&2
  exit 1
fi

step "repeat build should reuse EROFS blobs"
cached_json="$("$kumabox_path" \
  --root-dir "$root_dir" \
  image build "$ref" \
  --name "$cleanup_name" \
  --platform "$platform" \
  --source "$source" \
  --mkfs-erofs "$mkfs_erofs" \
  --json)"
printf '%s\n' "$cached_json"

first_paths="$(printf '%s' "$build_json" | jq -r '[.oci.layers[].erofs.path] | join("\n")')"
cached_paths="$(printf '%s' "$cached_json" | jq -r '[.oci.layers[].erofs.path] | join("\n")')"
if [[ "$first_paths" != "$cached_paths" ]]; then
  echo "EROFS paths changed across repeated builds" >&2
  exit 1
fi

step "cleanup verification image records"
"$kumabox_path" --root-dir "$root_dir" image rm "$cleanup_name" >/dev/null
"$kumabox_path" --root-dir "$root_dir" image rm "$name" >/dev/null

echo "P3-03 OCI EROFS builder verification passed"
