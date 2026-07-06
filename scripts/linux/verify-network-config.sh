#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
cloud_hypervisor_path="cloud-hypervisor"
qemu_img_path="qemu-img"
root_dir="/tmp/kumabox-p0/data"
run_dir="/tmp/kumabox-p0/run"
log_dir="/tmp/kumabox-p0/logs"
use_sudo=0

usage() {
  cat <<'USAGE'
Usage: scripts/linux/verify-network-config.sh [options]

Verifies P2-01 network config and capability plumbing. This script does not
create tap devices, bridges, NAT rules, or VMs.

Options:
  --kumabox PATH             kumabox binary path
  --cloud-hypervisor PATH    cloud-hypervisor binary or command
  --qemu-img PATH            qemu-img binary or command
  --root-dir PATH            persistent state directory
  --run-dir PATH             runtime directory
  --log-dir PATH             log directory
  --sudo                     run kumabox doctor through sudo
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --kumabox)
      kumabox_path="${2:-}"
      shift 2
      ;;
    --cloud-hypervisor)
      cloud_hypervisor_path="${2:-}"
      shift 2
      ;;
    --qemu-img)
      qemu_img_path="${2:-}"
      shift 2
      ;;
    --root-dir)
      root_dir="${2:-}"
      shift 2
      ;;
    --run-dir)
      run_dir="${2:-}"
      shift 2
      ;;
    --log-dir)
      log_dir="${2:-}"
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

if [[ -z "$kumabox_path" || -z "$root_dir" || -z "$run_dir" || -z "$log_dir" ]]; then
  usage >&2
  exit 2
fi

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "verify-network-config must run on Linux" >&2
  exit 1
fi

if [[ "$use_sudo" -eq 1 ]]; then
  if ! command -v sudo >/dev/null 2>&1; then
    echo "--sudo requested but sudo is missing" >&2
    exit 1
  fi
  kumabox_cmd=(sudo "$kumabox_path")
else
  kumabox_cmd=("$kumabox_path")
fi

rm -rf "$root_dir/network"
mkdir -p "$root_dir" "$run_dir" "$log_dir"

scripts/linux/env-check.sh \
  --kumabox "$kumabox_path" \
  --cloud-hypervisor "$cloud_hypervisor_path" \
  --qemu-img "$qemu_img_path" \
  --strict \
  --network

doctor_json="$("${kumabox_cmd[@]}" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  doctor --json)"

printf '%s\n' "$doctor_json" | grep -q '"name": "networkProvider"'
printf '%s\n' "$doctor_json" | grep -q '"name": "networkTun"'
printf '%s\n' "$doctor_json" | grep -q '"name": "networkIPCommand"'
printf '%s\n' "$doctor_json" | grep -q '"name": "networkNAT"'
printf '%s\n' "$doctor_json" | grep -q '"name": "networkPermission"'

network_ls="$("${kumabox_cmd[@]}" \
  --root-dir "$root_dir" \
  network ls --json)"
if [[ "$network_ls" != "[]" ]]; then
  echo "network ls expected empty JSON list, got: $network_ls" >&2
  exit 1
fi

network_inspect="$("${kumabox_cmd[@]}" \
  --root-dir "$root_dir" \
  network inspect kb_missing --json)"
printf '%s\n' "$network_inspect" | grep -q '"vmId": "kb_missing"'
printf '%s\n' "$network_inspect" | grep -q '"interfaces": \[\]'

echo "P2-01 network config verification passed"
