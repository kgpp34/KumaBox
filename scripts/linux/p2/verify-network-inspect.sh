#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
cloud_hypervisor_path="cloud-hypervisor"
qemu_img_path="qemu-img"
root_dir="/tmp/kumabox-p0/data"
run_dir="/tmp/kumabox-p0/run"
log_dir="/tmp/kumabox-p0/logs"
name="p2-inspect"
root_disk=""
firmware=""
use_sudo=0
tap=""
vm_exists=0

usage() {
  cat <<'USAGE'
Usage: scripts/linux/p2/verify-network-inspect.sh --root-disk PATH --firmware PATH [options]

Verifies P2-05 network provider inspect output. This script creates a VM
record with --network default, compares provider index and VM networkConfigs,
then intentionally introduces provider-index drift and verifies it is reported.
It does not start the VM.

Options:
  --kumabox PATH             kumabox binary path
  --cloud-hypervisor PATH    cloud-hypervisor binary or command
  --qemu-img PATH            qemu-img binary or command
  --root-dir PATH            persistent state directory
  --run-dir PATH             runtime directory
  --log-dir PATH             log directory
  --name NAME                VM name
  --root-disk PATH           cloud image root disk path
  --firmware PATH            UEFI firmware path
  --sudo                     run network operations through sudo
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

jq_file() {
  local filter="$1"
  local path="$2"
  "${cat_cmd[@]}" "$path" | jq "$filter"
}

write_jq_file() {
  local filter="$1"
  local path="$2"
  local tmp
  tmp="$(mktemp)"
  "${cat_cmd[@]}" "$path" | jq "$filter" >"$tmp"
  "${copy_cmd[@]}" "$tmp" "$path"
  rm -f "$tmp"
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
  echo "verify-network-inspect must run on Linux" >&2
  exit 1
fi

for bin in jq; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "$bin is required" >&2
    exit 1
  fi
done

if [[ "$use_sudo" -eq 1 ]]; then
  if ! command -v sudo >/dev/null 2>&1; then
    echo "--sudo requested but sudo is missing" >&2
    exit 1
  fi
  kumabox_cmd=(sudo "$kumabox_path")
  remove_cmd=(sudo rm -rf)
  ip_cmd=(sudo ip)
  cat_cmd=(sudo cat)
  copy_cmd=(sudo cp)
else
  kumabox_cmd=("$kumabox_path")
  remove_cmd=(rm -rf)
  ip_cmd=(ip)
  cat_cmd=(cat)
  copy_cmd=(cp)
fi

cleanup() {
  set +e
  if [[ "$vm_exists" -eq 1 ]]; then
    "${kumabox_cmd[@]}" \
      --root-dir "$root_dir" \
      --run-dir "$run_dir" \
      --log-dir "$log_dir" \
      --cloud-hypervisor-bin "$cloud_hypervisor_path" \
      delete "$name" --force >/dev/null 2>&1
  fi
  if [[ -n "$tap" && "$tap" != "null" ]]; then
    "${ip_cmd[@]}" link delete "$tap" >/dev/null 2>&1
  fi
  "${kumabox_cmd[@]}" \
    --root-dir "$root_dir" \
    --run-dir "$run_dir" \
    --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor_path" \
    network teardown --json >/dev/null 2>&1
  "${remove_cmd[@]}" "$root_dir" "$run_dir" "$log_dir"
}
trap cleanup EXIT

section "clean previous P2-05 state"
"${remove_cmd[@]}" "$root_dir" "$run_dir" "$log_dir"
mkdir -p "$root_dir" "$run_dir" "$log_dir"

section "environment checks"
scripts/linux/env-check.sh \
  --kumabox "$kumabox_path" \
  --cloud-hypervisor "$cloud_hypervisor_path" \
  --qemu-img "$qemu_img_path" \
  --strict \
  --network

section "create VM with --network default"
created_json="$("${kumabox_cmd[@]}" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  create \
  --name "$name" \
  --root-disk "$root_disk" \
  --firmware "$firmware" \
  --network default)"
vm_exists=1
printf '%s\n' "$created_json"

vm_id="$(printf '%s' "$created_json" | jq -r '.id')"
net_id="$(printf '%s' "$created_json" | jq -r '.networkConfigs[0].id')"
tap="$(printf '%s' "$created_json" | jq -r '.networkConfigs[0].tap')"
mac="$(printf '%s' "$created_json" | jq -r '.networkConfigs[0].mac')"
ip_addr="$(printf '%s' "$created_json" | jq -r '.networkConfigs[0].network.ip')"

section "network inspect by VM name"
network_inspect="$("${kumabox_cmd[@]}" \
  --root-dir "$root_dir" \
  network inspect "$name" --json)"
printf '%s\n' "$network_inspect"
if [[ "$(printf '%s' "$network_inspect" | jq -r '.vmId')" != "$vm_id" ]]; then
  echo "network inspect did not resolve VM name to id $vm_id" >&2
  exit 1
fi
if [[ "$(printf '%s' "$network_inspect" | jq -r '.interfaces[0].id')" != "$net_id" ]]; then
  echo "network inspect missing provider record $net_id" >&2
  exit 1
fi
if [[ "$(printf '%s' "$network_inspect" | jq -r '.vmConfigs[0].id')" != "$net_id" ]]; then
  echo "network inspect missing VM network config $net_id" >&2
  exit 1
fi
if [[ "$(printf '%s' "$network_inspect" | jq -r '.drift | length')" != "0" ]]; then
  echo "network inspect unexpectedly reported drift" >&2
  exit 1
fi
printf 'state: network inspect tap=%s mac=%s ip=%s drift=0\n' "$tap" "$mac" "$ip_addr"

section "VM inspect networkStatus"
vm_inspect="$("${kumabox_cmd[@]}" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  inspect "$name" --json)"
printf '%s\n' "$vm_inspect" | jq '.networkStatus'
if [[ "$(printf '%s' "$vm_inspect" | jq -r '.networkStatus.interfaces[0].id')" != "$net_id" ]]; then
  echo "VM inspect networkStatus missing provider record $net_id" >&2
  exit 1
fi

section "provider index before drift"
jq_file '.' "$root_dir/network/index.json"

section "introduce provider index drift"
write_jq_file '(.networks[] | select(.vmId == "'"$vm_id"'") | .mac) = "5a:ff:ff:ff:ff:ff"' "$root_dir/network/index.json"
jq_file '.' "$root_dir/network/index.json"

section "network inspect reports drift"
drift_json="$("${kumabox_cmd[@]}" \
  --root-dir "$root_dir" \
  network inspect "$name" --json)"
printf '%s\n' "$drift_json"
if [[ "$(printf '%s' "$drift_json" | jq -r '.drift | length')" -lt 1 ]]; then
  echo "network inspect did not report drift after provider index mutation" >&2
  exit 1
fi
printf '%s\n' "$drift_json" | jq -e '.drift[] | select(contains("mac mismatch"))' >/dev/null

echo "P2-05 network inspect verification passed"
