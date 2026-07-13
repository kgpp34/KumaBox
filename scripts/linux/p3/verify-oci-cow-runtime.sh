#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
root_dir="/tmp/kumabox-p0/data"
run_dir="/tmp/kumabox-p0/run"
log_dir="/tmp/kumabox-p0/logs"
ref="kumabox/ubuntu:24.04-p3"
platform="linux/amd64"
image_name="p3-cow-image"
vm_name="p3-cow"
storage_size="64M"
source="auto"
mkfs_erofs="mkfs.erofs"

usage() {
  cat <<'USAGE'
Usage: scripts/linux/p3/verify-oci-cow-runtime.sh [options]

Options:
  --kumabox PATH      kumabox binary path, defaults to ./bin/kumabox
  --root-dir PATH     state root directory, defaults to /tmp/kumabox-p0/data
  --run-dir PATH      runtime directory, defaults to /tmp/kumabox-p0/run
  --log-dir PATH      log directory, defaults to /tmp/kumabox-p0/logs
  --ref REF           OCI image ref, defaults to kumabox/ubuntu:24.04-p3
  --platform VALUE    OCI platform, defaults to linux/amd64
  --image-name NAME   built image name, defaults to p3-cow-image
  --name NAME         VM name, defaults to p3-cow
  --storage SIZE      per-VM COW size, defaults to 64M
  --source VALUE      OCI source: auto, registry, or daemon. Defaults to auto
  --mkfs-erofs PATH   mkfs.erofs binary path, defaults to mkfs.erofs

Verifies OCI COW runtime rendering:
build OCI image -> create VM from image -> create per-VM ext4 COW -> render
readonly EROFS layer disks and writable COW disk into Cloud Hypervisor config.
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
    --run-dir)
      require_value "$1" "${2:-}"
      run_dir="$2"
      shift 2
      ;;
    --log-dir)
      require_value "$1" "${2:-}"
      log_dir="$2"
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
    --image-name)
      require_value "$1" "${2:-}"
      image_name="$2"
      shift 2
      ;;
    --name)
      require_value "$1" "${2:-}"
      vm_name="$2"
      shift 2
      ;;
    --storage)
      require_value "$1" "${2:-}"
      storage_size="$2"
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
for bin in jq mkfs.ext4; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "$bin is required for COW runtime verification" >&2
    exit 1
  fi
done
if [[ "$mkfs_erofs" == */* ]]; then
  if [[ ! -x "$mkfs_erofs" ]]; then
    echo "mkfs.erofs is not executable: $mkfs_erofs" >&2
    exit 1
  fi
elif ! command -v "$mkfs_erofs" >/dev/null 2>&1; then
  echo "mkfs.erofs is required for COW runtime verification" >&2
  exit 1
fi

kb() {
  "$kumabox_path" --root-dir "$root_dir" --run-dir "$run_dir" --log-dir "$log_dir" "$@"
}

step "clean previous OCI COW state"
kb delete "$vm_name" --force >/dev/null 2>&1 || true
kb image rm "$image_name" >/dev/null 2>&1 || true

step "build OCI image"
image_json="$(kb image build "$ref" \
  --name "$image_name" \
  --platform "$platform" \
  --source "$source" \
  --mkfs-erofs "$mkfs_erofs" \
  --json)"
printf '%s\n' "$image_json"

step "create VM from OCI image"
vm_json="$(kb create "$image_name" \
  --name "$vm_name" \
  --storage "$storage_size" \
  --network none)"
printf '%s\n' "$vm_json"

vm_id="$(printf '%s' "$vm_json" | jq -r '.id')"
config_path="$(printf '%s' "$vm_json" | jq -r '.config')"
cow_path="$(printf '%s' "$vm_json" | jq -r '.storageConfigs[] | select(.type=="cow") | .path')"
layer_count="$(printf '%s' "$vm_json" | jq '[.storageConfigs[] | select(.type=="layer")] | length')"

if [[ "$vm_id" == "" || "$vm_id" == "null" ]]; then
  echo "VM id missing" >&2
  exit 1
fi
if [[ "$layer_count" -lt 1 ]]; then
  echo "expected at least one layer storage config" >&2
  exit 1
fi
if [[ ! -s "$cow_path" ]]; then
  echo "COW disk missing or empty: $cow_path" >&2
  exit 1
fi
printf 'state: vm=%s layers=%s cow=%s size=%s config=%s\n' "$vm_id" "$layer_count" "$cow_path" "$(stat -c '%s' "$cow_path")" "$config_path"

step "inspect rendered Cloud Hypervisor config"
if [[ ! -s "$config_path" ]]; then
  echo "rendered config missing: $config_path" >&2
  exit 1
fi
jq '.disks, .kernel' "$config_path"

rendered_layers="$(jq '[.disks[] | select(.readonly == true and (.serial | startswith("kumabox-layer")))] | length' "$config_path")"
rendered_cow="$(jq '[.disks[] | select(.readonly != true and .serial == "kumabox-cow")] | length' "$config_path")"
cmdline="$(jq -r '.kernel.cmdline' "$config_path")"

if [[ "$rendered_layers" -ne "$layer_count" ]]; then
  echo "rendered layer disk count mismatch: got $rendered_layers want $layer_count" >&2
  exit 1
fi
if [[ "$rendered_cow" -ne 1 ]]; then
  echo "rendered COW disk missing" >&2
  exit 1
fi
if [[ "$cmdline" != *"kumabox.layers=kumabox-layer0"* || "$cmdline" != *"kumabox.cow=kumabox-cow"* ]]; then
  echo "kernel cmdline missing storage serials: $cmdline" >&2
  exit 1
fi
if [[ "$cmdline" != *"boot=kumabox-overlay"* || "$cmdline" == *"root=/dev/ram0"* ]]; then
  echo "kernel cmdline does not select KumaBox overlay boot: $cmdline" >&2
  exit 1
fi
printf 'state: cmdline=%s\n' "$cmdline"

step "delete VM and verify COW cleanup"
delete_json="$(kb delete "$vm_name")"
printf '%s\n' "$delete_json"
if [[ -e "$cow_path" ]]; then
  echo "COW disk was not cleaned up: $cow_path" >&2
  exit 1
fi
printf 'state: COW cleaned up: %s\n' "$cow_path"

step "cleanup image record"
kb image rm "$image_name" >/dev/null

echo "P3-05 OCI COW runtime verification passed"
