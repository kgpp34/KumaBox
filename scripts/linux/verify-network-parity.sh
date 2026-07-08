#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
kumabox_path="./bin/kumabox"
cloud_hypervisor_path="cloud-hypervisor"
qemu_img_path="qemu-img"
root_dir="/tmp/kumabox-p0/data"
run_dir="/tmp/kumabox-p0/run"
log_dir="/tmp/kumabox-p0/logs"
root_disk=""
firmware=""
use_sudo=0

usage() {
  cat <<'USAGE'
Usage: scripts/linux/verify-network-parity.sh --root-disk PATH --firmware PATH [options]

Runs the P2-12 network parity matrix:
  none     0 NIC
  host-tap 1 NIC
  host-tap 2 NICs
  CNI      1 NIC with mock plugin
  CNI      2 NICs with mock plugin
  queues   vCPU-based queue strategy

Options:
  --kumabox PATH             kumabox binary path
  --cloud-hypervisor PATH    cloud-hypervisor binary or command
  --qemu-img PATH            qemu-img binary or command
  --root-dir PATH            persistent state directory
  --run-dir PATH             runtime directory
  --log-dir PATH             log directory
  --root-disk PATH           cloud image root disk path
  --firmware PATH            UEFI firmware path
  --sudo                     pass --sudo to child scripts
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

section() {
  printf '\n==> %s\n' "$1"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --kumabox)
      require_value "$1" "${2:-}"
      kumabox_path="$2"
      shift 2
      ;;
    --cloud-hypervisor)
      require_value "$1" "${2:-}"
      cloud_hypervisor_path="$2"
      shift 2
      ;;
    --qemu-img)
      require_value "$1" "${2:-}"
      qemu_img_path="$2"
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
    --sudo)
      use_sudo=1
      shift
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

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "verify-network-parity must run on Linux" >&2
  exit 1
fi

if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required" >&2
  exit 1
fi

sudo_arg=()
if [[ "$use_sudo" -eq 1 ]]; then
  sudo_arg=(--sudo)
  kumabox_cmd=(sudo "$kumabox_path")
  remove_cmd=(sudo rm -rf)
  mkdir_cmd=(sudo mkdir -p)
else
  kumabox_cmd=("$kumabox_path")
  remove_cmd=(rm -rf)
  mkdir_cmd=(mkdir -p)
fi

common_args=(
  --kumabox "$kumabox_path"
  --cloud-hypervisor "$cloud_hypervisor_path"
  --qemu-img "$qemu_img_path"
  --root-dir "$root_dir"
  --run-dir "$run_dir"
  --log-dir "$log_dir"
  --root-disk "$root_disk"
  --firmware "$firmware"
  "${sudo_arg[@]}"
)

cleanup_dirs() {
  "${remove_cmd[@]}" "$root_dir" "$run_dir" "$log_dir"
  "${mkdir_cmd[@]}" "$root_dir" "$run_dir" "$log_dir"
}

section "none provider: 0 NIC"
cleanup_dirs
none_json="$("${kumabox_cmd[@]}" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  create \
  --name p2-parity-none \
  --root-disk "$root_disk" \
  --firmware "$firmware" \
  --network none)"
printf '%s\n' "$none_json"
if [[ "$(printf '%s' "$none_json" | jq -r '.network')" != "none" ]]; then
  echo "none VM network should be none" >&2
  exit 1
fi
if [[ "$(printf '%s' "$none_json" | jq -r '.networkConfigs | length')" != "0" ]]; then
  echo "none VM should not have networkConfigs" >&2
  exit 1
fi
if [[ "$("${kumabox_cmd[@]}" --root-dir "$root_dir" network ls --json | jq 'length')" != "0" ]]; then
  echo "none VM should not create provider records" >&2
  exit 1
fi
"${kumabox_cmd[@]}" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  delete p2-parity-none --force >/dev/null

section "host-tap provider: 1 NIC"
"$script_dir/verify-network-e2e.sh" "${common_args[@]}" --name p2-parity-hosttap1

section "host-tap provider: 2 NICs"
"$script_dir/verify-network-multinic.sh" "${common_args[@]}" --name p2-parity-hosttap2

section "CNI provider: 1 NIC"
"$script_dir/verify-cni-provider.sh" "${common_args[@]}" --name p2-parity-cni1

section "CNI provider: 2 NICs"
"$script_dir/verify-cni-multinic.sh" "${common_args[@]}" --name p2-parity-cni2

section "queue strategy"
"$script_dir/verify-network-queues.sh" "${common_args[@]}" --name-prefix p2-parity-queues

echo "P2-12 network parity matrix verification passed"
