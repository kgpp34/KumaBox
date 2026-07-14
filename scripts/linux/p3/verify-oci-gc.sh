#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
root_dir="/tmp/kumabox-p0/data"
run_dir="/tmp/kumabox-p0/run"
log_dir="/tmp/kumabox-p0/logs"

usage() {
  cat <<'USAGE'
Usage: scripts/linux/p3/verify-oci-gc.sh [options]

Options:
  --kumabox PATH   kumabox binary path, defaults to ./bin/kumabox
  --root-dir PATH  state root directory, defaults to /tmp/kumabox-p0/data
  --run-dir PATH   runtime directory, defaults to /tmp/kumabox-p0/run
  --log-dir PATH   log directory, defaults to /tmp/kumabox-p0/logs

Verifies OCI GC dry-run reporting for content staging, build staging,
orphan content blobs, orphan EROFS blobs, and orphan boot assets.
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
    --kumabox) require_value "$1" "${2:-}"; kumabox_path="$2"; shift 2 ;;
    --root-dir) require_value "$1" "${2:-}"; root_dir="$2"; shift 2 ;;
    --run-dir) require_value "$1" "${2:-}"; run_dir="$2"; shift 2 ;;
    --log-dir) require_value "$1" "${2:-}"; log_dir="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [[ ! -x "$kumabox_path" ]]; then
  echo "kumabox is not executable: $kumabox_path" >&2
  exit 1
fi
if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required for OCI GC verification" >&2
  exit 1
fi

kb() {
  "$kumabox_path" \
    --root-dir "$root_dir" \
    --run-dir "$run_dir" \
    --log-dir "$log_dir" \
    "$@"
}

step "clean previous OCI GC state"
rm -rf "$root_dir" "$run_dir" "$log_dir"
mkdir -p "$root_dir" "$run_dir" "$log_dir"

content_blob="$root_dir/oci/content/blobs/sha256/$(printf '7%.0s' {1..64})"
content_stage="$root_dir/oci/content/staging/blob-deadbeef"
build_stage="$root_dir/oci/staging/erofs-deadbeef/layer.tar"
erofs_blob="$root_dir/oci/erofs/blobs/sha256/$(printf '5%.0s' {1..64}).erofs"
boot_asset="$root_dir/oci/boot/blobs/sha256/$(printf '3%.0s' {1..64})"

step "create orphan OCI artifacts"
for path in "$content_blob" "$content_stage" "$build_stage" "$erofs_blob" "$boot_asset"; do
  mkdir -p "$(dirname "$path")"
  printf 'orphan\n' >"$path"
  printf 'state: wrote %s\n' "$path"
done

step "run gc dry-run"
report="$(kb gc --dry-run --json)"
printf '%s\n' "$report" | jq '.'

require_candidate() {
  local path="$1"
  local type="$2"
  if ! printf '%s\n' "$report" | jq -e --arg path "$path" --arg type "$type" \
    '.candidates[] | select(.path == $path and .type == $type)' >/dev/null; then
    echo "missing GC candidate type=$type path=$path" >&2
    exit 1
  fi
  printf 'pass: gc candidate type=%s path=%s\n' "$type" "$path"
}

require_candidate "$content_blob" "orphan_content_blob"
require_candidate "$content_stage" "oci_content_staging"
require_candidate "$(dirname "$build_stage")" "oci_build_staging"
require_candidate "$erofs_blob" "orphan_erofs_blob"
require_candidate "$boot_asset" "orphan_boot_asset"

echo "P3-10 OCI GC dry-run verification passed"
