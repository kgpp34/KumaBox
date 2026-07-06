#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
cloud_hypervisor_path="cloud-hypervisor"
qemu_img_path="qemu-img"
root_dir="/tmp/kumabox-p0/data"
run_dir="/tmp/kumabox-p0/run"
log_dir="/tmp/kumabox-p0/logs"
name="p2-render"
root_disk=""
firmware=""
use_sudo=0
tap=""
vm_exists=0

usage() {
  cat <<'USAGE'
Usage: scripts/linux/verify-network-render.sh --root-disk PATH --firmware PATH [options]

Verifies P2-04 VM network attach and Cloud Hypervisor rendering. This script
creates a VM record with --network default, verifies tap/bridge/config/cidata,
then removes the VM record and host network artifacts. It does not start the VM.

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

jq_file_raw() {
  local filter="$1"
  local path="$2"
  "${cat_cmd[@]}" "$path" | jq -r "$filter"
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
  echo "verify-network-render must run on Linux" >&2
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
else
  kumabox_cmd=("$kumabox_path")
  remove_cmd=(rm -rf)
  ip_cmd=(ip)
  cat_cmd=(cat)
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

section "clean previous P2-04 state"
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

tap="$(printf '%s' "$created_json" | jq -r '.networkConfigs[0].tap')"
mac="$(printf '%s' "$created_json" | jq -r '.networkConfigs[0].mac')"
ip_addr="$(printf '%s' "$created_json" | jq -r '.networkConfigs[0].network.ip')"
config_path="$(printf '%s' "$created_json" | jq -r '.config')"
cidata_dir="$(printf '%s' "$created_json" | jq -r '.metadata.cidataDir')"

if [[ -z "$tap" || "$tap" == "null" ]]; then
  echo "missing networkConfigs[0].tap" >&2
  exit 1
fi
if [[ "$ip_addr" != 10.88.* ]]; then
  echo "unexpected network IP: $ip_addr" >&2
  exit 1
fi
printf 'state: vm network tap=%s mac=%s ip=%s config=%s cidata=%s\n' "$tap" "$mac" "$ip_addr" "$config_path" "$cidata_dir"

section "host tap link"
"${ip_cmd[@]}" link show dev "$tap" >/dev/null
"${ip_cmd[@]}" -d link show dev "$tap"
printf 'note: host tap link/ether may differ from VM MAC; guest MAC is rendered in Cloud Hypervisor config below.\n'
master="$(basename "$(readlink "/sys/class/net/$tap/master")")"
if [[ "$master" != "kumabox0" ]]; then
  echo "tap $tap master = $master, want kumabox0" >&2
  exit 1
fi
printf 'state: tap %s master=%s\n' "$tap" "$master"

section "bridge state"
"${ip_cmd[@]}" -d link show dev kumabox0
"${ip_cmd[@]}" -4 addr show dev kumabox0

section "cloud-hypervisor net config"
jq_file '.nets' "$config_path"
if [[ "$(jq_file_raw '.nets[0].tap' "$config_path")" != "$tap" ]]; then
  echo "Cloud Hypervisor config missing tap $tap" >&2
  jq_file '.nets' "$config_path" >&2
  exit 1
fi
if [[ "$(jq_file_raw '.nets[0].mac' "$config_path")" != "$mac" ]]; then
  echo "Cloud Hypervisor config missing mac $mac" >&2
  jq_file '.nets' "$config_path" >&2
  exit 1
fi

section "cidata network-config"
network_config="$("${cat_cmd[@]}" "$cidata_dir/network-config")"
printf '%s\n' "$network_config"
printf '%s\n' "$network_config" | grep -q "macaddress: \"$mac\""
printf '%s\n' "$network_config" | grep -q "$ip_addr/16"
printf '%s\n' "$network_config" | grep -q "gateway4: 10.88.0.1"

section "network provider index"
jq_file '.' "$root_dir/network/index.json"
provider_tap="$(jq_file_raw '.networks[] | .tap' "$root_dir/network/index.json")"
if [[ "$provider_tap" != "$tap" ]]; then
  echo "provider index tap = $provider_tap, want $tap" >&2
  exit 1
fi

section "host-tap owner state"
jq_file '.' "$root_dir/network/host-tap.json"

section "lease state"
jq_file '.' "$root_dir/network/leases.json"

echo "P2-04 network render verification passed"
