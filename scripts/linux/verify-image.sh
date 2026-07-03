#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
root_dir="/tmp/kumabox-p0/data"
qemu_img_path="qemu-img"
name="p1-image"
root_disk=""
firmware=""

usage() {
  cat <<'USAGE'
Usage: scripts/linux/verify-image.sh --root-disk PATH --firmware PATH [options]

Options:
  --kumabox PATH    kumabox binary path, defaults to ./bin/kumabox
  --root-dir PATH   image store root directory, defaults to /tmp/kumabox-p0/data
  --qemu-img PATH   qemu-img binary path, defaults to qemu-img
  --name NAME       image name, defaults to p1-image

Runs the P1-02 local image import path:
image import -> image inspect -> manifest/source/base disk checks.
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

abs_path() {
  local path="$1"
  local dir
  local base
  dir="$(cd "$(dirname "$path")" && pwd -P)"
  base="$(basename "$path")"
  printf '%s/%s\n' "$dir" "$base"
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
    --qemu-img)
      require_value "$1" "${2:-}"
      qemu_img_path="$2"
      shift 2
      ;;
    --name)
      require_value "$1" "${2:-}"
      name="$2"
      shift 2
      ;;
    --root-disk)
      require_value "$1" "${2:-}"
      root_disk="$2"
      shift 2
      ;;
    --firmware)
      require_value "$1" "${2:-}"
      firmware="$2"
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

if [[ -z "$root_disk" || -z "$firmware" ]]; then
  usage >&2
  exit 2
fi

for path in "$kumabox_path" "$root_disk" "$firmware"; do
  if [[ ! -e "$path" ]]; then
    echo "required path does not exist: $path" >&2
    exit 1
  fi
done

root_disk="$(abs_path "$root_disk")"
firmware="$(abs_path "$firmware")"

if [[ ! -x "$kumabox_path" ]]; then
  echo "kumabox is not executable: $kumabox_path" >&2
  exit 1
fi

if [[ "$qemu_img_path" == */* ]]; then
  if [[ ! -x "$qemu_img_path" ]]; then
    echo "qemu-img is not executable: $qemu_img_path" >&2
    exit 1
  fi
elif ! command -v "$qemu_img_path" >/dev/null 2>&1; then
  echo "qemu-img not found: $qemu_img_path" >&2
  exit 1
fi

import_json="$("$kumabox_path" \
  --root-dir "$root_dir" \
  image import "$root_disk" \
  --name "$name" \
  --firmware "$firmware" \
  --qemu-img "$qemu_img_path")"

printf '%s\n' "$import_json"

image_id="$(printf '%s' "$import_json" | sed -n 's/.*"id": "\([^"]*\)".*/\1/p' | head -n 1)"
base_disk="$(printf '%s' "$import_json" | sed -n 's/.*"path": "\([^"]*\)".*/\1/p' | head -n 1)"
format="$(printf '%s' "$import_json" | sed -n 's/.*"format": "\([^"]*\)".*/\1/p' | head -n 1)"
sha256="$(printf '%s' "$import_json" | sed -n 's/.*"sha256": "\([^"]*\)".*/\1/p' | head -n 1)"

if [[ -z "$image_id" ]]; then
  echo "could not parse image id from import output" >&2
  exit 1
fi

if [[ -z "$base_disk" || ! -f "$base_disk" ]]; then
  echo "managed base disk is missing: $base_disk" >&2
  exit 1
fi

if [[ -z "$format" || -z "$sha256" ]]; then
  echo "import output is missing format or sha256" >&2
  exit 1
fi

image_dir="$(dirname "$base_disk")"
for manifest in "$image_dir/image.json" "$image_dir/source.json"; do
  if [[ ! -f "$manifest" ]]; then
    echo "manifest is missing: $manifest" >&2
    exit 1
  fi
done

inspect_json="$("$kumabox_path" \
  --root-dir "$root_dir" \
  image inspect "$name" --json)"

printf '%s\n' "$inspect_json"

if ! printf '%s' "$inspect_json" | grep -q "\"id\": \"$image_id\""; then
  echo "image inspect did not return imported image id" >&2
  exit 1
fi

if ! printf '%s' "$inspect_json" | grep -q "\"firmware\": \"$firmware\""; then
  echo "image inspect did not preserve firmware path" >&2
  exit 1
fi

if [[ ! -f "$root_disk" ]]; then
  echo "source image was deleted unexpectedly: $root_disk" >&2
  exit 1
fi

echo "P1-02 image import verification passed"
