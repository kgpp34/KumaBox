#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
cloud_hypervisor_path="cloud-hypervisor"
qemu_img_path="qemu-img"
root_dir="/tmp/kumabox-p0/data"
run_dir="/tmp/kumabox-p0/run"
log_dir="/tmp/kumabox-p0/logs"
ref="kumabox/ubuntu:24.04-p3"
platform="linux/amd64"
image_name="p3-boot-image"
vm_name="p3-boot"
storage_size="64M"
source="auto"
mkfs_erofs="mkfs.erofs"
use_sudo=false
wait_seconds=60

usage() {
  cat <<'USAGE'
Usage: scripts/linux/verify-oci-direct-boot-network.sh [options]

Options:
  --kumabox PATH             kumabox binary path, defaults to ./bin/kumabox
  --cloud-hypervisor PATH    cloud-hypervisor path, defaults to cloud-hypervisor
  --qemu-img PATH            qemu-img path, defaults to qemu-img
  --root-dir PATH            state root directory, defaults to /tmp/kumabox-p0/data
  --run-dir PATH             runtime directory, defaults to /tmp/kumabox-p0/run
  --log-dir PATH             log directory, defaults to /tmp/kumabox-p0/logs
  --ref REF                  OCI image ref, defaults to kumabox/ubuntu:24.04-p3
  --platform VALUE           OCI platform, defaults to linux/amd64
  --image-name NAME          built image name, defaults to p3-boot-image
  --name NAME                VM name, defaults to p3-boot
  --storage SIZE             per-VM COW size, defaults to 64M
  --source VALUE             OCI source: auto, registry, or daemon. Defaults to auto
  --mkfs-erofs PATH          mkfs.erofs binary path, defaults to mkfs.erofs
  --wait-seconds N           seconds to keep VM alive for console collection, defaults to 60
  --sudo                     use sudo for host network cleanup in env-check

Verifies OCI direct boot network rendering and smoke startup:
build OCI image -> run VM with --network default -> verify cmdline has layer/COW,
hostname, and static IP parameters -> show console tail -> delete VM.
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
    --cloud-hypervisor) require_value "$1" "${2:-}"; cloud_hypervisor_path="$2"; shift 2 ;;
    --qemu-img) require_value "$1" "${2:-}"; qemu_img_path="$2"; shift 2 ;;
    --root-dir) require_value "$1" "${2:-}"; root_dir="$2"; shift 2 ;;
    --run-dir) require_value "$1" "${2:-}"; run_dir="$2"; shift 2 ;;
    --log-dir) require_value "$1" "${2:-}"; log_dir="$2"; shift 2 ;;
    --ref) require_value "$1" "${2:-}"; ref="$2"; shift 2 ;;
    --platform) require_value "$1" "${2:-}"; platform="$2"; shift 2 ;;
    --image-name) require_value "$1" "${2:-}"; image_name="$2"; shift 2 ;;
    --name) require_value "$1" "${2:-}"; vm_name="$2"; shift 2 ;;
    --storage) require_value "$1" "${2:-}"; storage_size="$2"; shift 2 ;;
    --source) require_value "$1" "${2:-}"; source="$2"; shift 2 ;;
    --mkfs-erofs) require_value "$1" "${2:-}"; mkfs_erofs="$2"; shift 2 ;;
    --wait-seconds) require_value "$1" "${2:-}"; wait_seconds="$2"; shift 2 ;;
    --sudo) use_sudo=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [[ ! -x "$kumabox_path" ]]; then
  echo "kumabox is not executable: $kumabox_path" >&2
  exit 1
fi
for bin in jq mkfs.ext4; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "$bin is required for OCI direct boot verification" >&2
    exit 1
  fi
done
if [[ "$mkfs_erofs" == */* ]]; then
  if [[ ! -x "$mkfs_erofs" ]]; then
    echo "mkfs.erofs is not executable: $mkfs_erofs" >&2
    exit 1
  fi
elif ! command -v "$mkfs_erofs" >/dev/null 2>&1; then
  echo "mkfs.erofs is required for OCI direct boot verification" >&2
  exit 1
fi

kb() {
  "$kumabox_path" \
    --root-dir "$root_dir" \
    --run-dir "$run_dir" \
    --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor_path" \
    "$@"
}

cleanup() {
  kb delete "$vm_name" --force >/dev/null 2>&1 || true
  kb image rm "$image_name" >/dev/null 2>&1 || true
}

step "clean previous OCI direct boot state"
cleanup

step "environment checks"
env_args=(
  --kumabox "$kumabox_path"
  --cloud-hypervisor "$cloud_hypervisor_path"
  --qemu-img "$qemu_img_path"
)
if [[ "$use_sudo" == true ]]; then
  env_args+=(--sudo)
fi
./scripts/linux/env-check.sh "${env_args[@]}"

step "build OCI image"
image_json="$(kb image build "$ref" \
  --name "$image_name" \
  --platform "$platform" \
  --source "$source" \
  --mkfs-erofs "$mkfs_erofs" \
  --json)"
printf '%s\n' "$image_json"

step "run OCI VM with --network default"
run_json="$(kb run "$image_name" \
  --name "$vm_name" \
  --storage "$storage_size" \
  --network default)"
printf '%s\n' "$run_json"

vm_id="$(printf '%s' "$run_json" | jq -r '.id')"
state="$(printf '%s' "$run_json" | jq -r '.state')"
config_path="$(printf '%s' "$run_json" | jq -r '.config')"
console_log="$(printf '%s' "$run_json" | jq -r '.logDir')/console.log"
guest_ip="$(printf '%s' "$run_json" | jq -r '.networkConfigs[0].network.ip')"

if [[ "$state" != "running" ]]; then
  echo "VM did not reach running state: $state" >&2
  exit 1
fi
printf 'state: vm=%s guest_ip=%s config=%s console=%s\n' "$vm_id" "$guest_ip" "$config_path" "$console_log"

step "network inspect"
kb network inspect "$vm_name" --json

step "rendered direct boot config"
jq '.kernel, .disks, .nets' "$config_path"
cmdline="$(jq -r '.kernel.cmdline' "$config_path")"
if [[ "$cmdline" != *"kumabox.layers=kumabox-layer0"* || "$cmdline" != *"kumabox.cow=kumabox-cow"* ]]; then
  echo "cmdline missing layer/COW serials: $cmdline" >&2
  exit 1
fi
if [[ "$cmdline" != *"kumabox.hostname=$vm_name"* ]]; then
  echo "cmdline missing hostname: $cmdline" >&2
  exit 1
fi
if [[ "$guest_ip" != "null" && "$guest_ip" != "" && "$cmdline" != *"ip=$guest_ip::"* ]]; then
  echo "cmdline missing guest IP $guest_ip: $cmdline" >&2
  exit 1
fi
printf 'state: cmdline=%s\n' "$cmdline"

step "wait for console output"
sleep "$wait_seconds"
if [[ -f "$console_log" ]]; then
  tail -n 120 "$console_log" || true
else
  echo "console log does not exist yet: $console_log"
fi

step "delete VM and cleanup image"
delete_json="$(kb delete "$vm_name" --force)"
printf '%s\n' "$delete_json"
kb image rm "$image_name" >/dev/null

echo "P3-06 OCI direct boot network verification passed"
