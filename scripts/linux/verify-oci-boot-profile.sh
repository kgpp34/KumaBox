#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
root_dir="/tmp/kumabox-p0/data"
ref="kumabox/ubuntu:24.04-p3"
platform="linux/amd64"
name="p3-boot"
source="auto"
mkfs_erofs="mkfs.erofs"

usage() {
  cat <<'USAGE'
Usage: scripts/linux/verify-oci-boot-profile.sh [options]

Options:
  --kumabox PATH      kumabox binary path, defaults to ./bin/kumabox
  --root-dir PATH     state root directory, defaults to /tmp/kumabox-p0/data
  --ref REF           OCI image ref, defaults to kumabox/ubuntu:24.04-p3
  --platform VALUE    OCI platform, defaults to linux/amd64
  --name NAME         image name, defaults to p3-boot
  --source VALUE      OCI source: auto, registry, or daemon. Defaults to auto
  --mkfs-erofs PATH   mkfs.erofs binary path, defaults to mkfs.erofs

Verifies OCI direct boot profile extraction:
image build extracts kernel/initrd from OCI layers into the boot asset store and
records a direct boot profile with the KumaBox overlay cmdline template.
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
  echo "jq is required for boot profile verification" >&2
  exit 1
fi
if [[ "$mkfs_erofs" == */* ]]; then
  if [[ ! -x "$mkfs_erofs" ]]; then
    echo "mkfs.erofs is not executable: $mkfs_erofs" >&2
    exit 1
  fi
elif ! command -v "$mkfs_erofs" >/dev/null 2>&1; then
  echo "mkfs.erofs is required for boot profile verification" >&2
  exit 1
fi

step "clean previous boot profile image record"
"$kumabox_path" --root-dir "$root_dir" image rm "$name" >/dev/null 2>&1 || true

step "build OCI image and extract boot profile"
build_json="$("$kumabox_path" \
  --root-dir "$root_dir" \
  image build "$ref" \
  --name "$name" \
  --platform "$platform" \
  --source "$source" \
  --mkfs-erofs "$mkfs_erofs" \
  --json)"
printf '%s\n' "$build_json"

boot_mode="$(printf '%s' "$build_json" | jq -r '.boot.mode')"
kernel_path="$(printf '%s' "$build_json" | jq -r '.boot.kernel')"
initrd_path="$(printf '%s' "$build_json" | jq -r '.boot.initrd')"
cmdline="$(printf '%s' "$build_json" | jq -r '.boot.cmdline')"

if [[ "$boot_mode" != "direct" ]]; then
  echo "unexpected boot mode: $boot_mode" >&2
  exit 1
fi
for path in "$kernel_path" "$initrd_path"; do
  if [[ ! -s "$path" ]]; then
    echo "boot asset missing or empty: $path" >&2
    exit 1
  fi
  printf 'state: bootAsset=%s size=%s\n' "$path" "$(stat -c '%s' "$path")"
done
if [[ "$cmdline" != *"kumabox.layers={{layers}}"* || "$cmdline" != *"kumabox.cow={{cow}}"* ]]; then
  echo "cmdline template missing overlay placeholders: $cmdline" >&2
  exit 1
fi
printf 'state: boot mode=%s kernel=%s initrd=%s\n' "$boot_mode" "$kernel_path" "$initrd_path"
printf 'state: cmdline=%s\n' "$cmdline"

step "inspect persisted boot profile"
inspect_json="$("$kumabox_path" --root-dir "$root_dir" image inspect "$name" --json)"
printf '%s\n' "$inspect_json"
inspect_kernel="$(printf '%s' "$inspect_json" | jq -r '.boot.kernel')"
inspect_initrd="$(printf '%s' "$inspect_json" | jq -r '.boot.initrd')"
if [[ "$inspect_kernel" != "$kernel_path" || "$inspect_initrd" != "$initrd_path" ]]; then
  echo "persisted boot profile does not match build output" >&2
  exit 1
fi

step "cleanup verification image record"
"$kumabox_path" --root-dir "$root_dir" image rm "$name" >/dev/null

echo "P3-04 OCI boot profile verification passed"
