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
Usage: scripts/linux/verify-hosttap-network.sh [options]

Verifies P2-03 host-tap bridge/NAT setup and cleanup. This script creates and
removes the KumaBox-owned bridge configured by kumabox, usually kumabox0.

Options:
  --kumabox PATH             kumabox binary path
  --cloud-hypervisor PATH    cloud-hypervisor binary or command
  --qemu-img PATH            qemu-img binary or command
  --root-dir PATH            persistent state directory
  --run-dir PATH             runtime directory
  --log-dir PATH             log directory
  --sudo                     run network setup/teardown through sudo
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

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "verify-hosttap-network must run on Linux" >&2
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

mkdir -p "$root_dir" "$run_dir" "$log_dir"

scripts/linux/env-check.sh \
  --kumabox "$kumabox_path" \
  --cloud-hypervisor "$cloud_hypervisor_path" \
  --qemu-img "$qemu_img_path" \
  --strict \
  --network

"${kumabox_cmd[@]}" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  network teardown --json >/dev/null || true

rm -rf "$root_dir/network"

setup_json="$("${kumabox_cmd[@]}" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  network setup --json)"

printf '%s\n' "$setup_json" | grep -q '"bridge": "kumabox0"'
printf '%s\n' "$setup_json" | grep -q '"gateway": "10.88.0.1"'

if [[ ! -f "$root_dir/network/host-tap.json" ]]; then
  echo "missing host-tap owner state: $root_dir/network/host-tap.json" >&2
  exit 1
fi

ip link show dev kumabox0 >/dev/null
ip -4 addr show dev kumabox0 | grep -q '10.88.0.1/16'

setup_again_json="$("${kumabox_cmd[@]}" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  network setup --json)"
printf '%s\n' "$setup_again_json" | grep -q '"bridge": "kumabox0"'

"${kumabox_cmd[@]}" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  network teardown --json >/dev/null

if ip link show dev kumabox0 >/dev/null 2>&1; then
  echo "kumabox0 still exists after teardown" >&2
  exit 1
fi
if [[ -f "$root_dir/network/host-tap.json" ]]; then
  echo "host-tap owner state still exists after teardown" >&2
  exit 1
fi

echo "P2-03 host-tap network verification passed"
