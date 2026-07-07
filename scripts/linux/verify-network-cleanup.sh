#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
cloud_hypervisor_path="cloud-hypervisor"
qemu_img_path="qemu-img"
root_dir="/tmp/kumabox-p0/data"
run_dir="/tmp/kumabox-p0/run"
log_dir="/tmp/kumabox-p0/logs"
name="p2-cleanup"
root_disk=""
firmware=""
use_sudo=0
tap=""
vm_exists=0

usage() {
  cat <<'USAGE'
Usage: scripts/linux/verify-network-cleanup.sh --root-disk PATH --firmware PATH [options]

Starts a real microVM with --network default, verifies stop keeps network
resources, then verifies delete releases the tap, IP lease, provider record,
and host-tap reference.

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

print_context() {
  set +e
  section "context: VM inspect"
  "${kumabox_cmd[@]}" \
    --root-dir "$root_dir" \
    --run-dir "$run_dir" \
    --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor_path" \
    inspect "$name" --json 2>/dev/null || true

  section "context: network inspect"
  "${kumabox_cmd[@]}" \
    --root-dir "$root_dir" \
    network inspect "$name" --json 2>/dev/null || true

  if [[ -f "$root_dir/network/index.json" ]]; then
    section "context: provider index"
    jq_file '.' "$root_dir/network/index.json" || true
  fi
  if [[ -f "$root_dir/network/leases.json" ]]; then
    section "context: IP leases"
    jq_file '.' "$root_dir/network/leases.json" || true
  fi
  if [[ -f "$root_dir/network/host-tap.json" ]]; then
    section "context: host-tap state"
    jq_file '.' "$root_dir/network/host-tap.json" || true
  fi
  if [[ -n "$tap" && "$tap" != "null" ]]; then
    section "context: host tap"
    "${ip_cmd[@]}" -d link show dev "$tap" 2>/dev/null || true
  fi
  section "context: bridge"
  "${ip_cmd[@]}" -d link show dev kumabox0 2>/dev/null || true
  "${ip_cmd[@]}" -4 addr show dev kumabox0 2>/dev/null || true
}

clean_previous_state() {
  section "clean previous P2-06 state"
  set +e

  old_taps=()
  if [[ -f "$root_dir/network/index.json" ]]; then
    mapfile -t old_taps < <("${cat_cmd[@]}" "$root_dir/network/index.json" 2>/dev/null | jq -r '.networks[]?.tap // empty' 2>/dev/null)
  fi

  "${kumabox_cmd[@]}" \
    --root-dir "$root_dir" \
    --run-dir "$run_dir" \
    --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor_path" \
    delete "$name" --force >/dev/null 2>&1

  for old_tap in "${old_taps[@]}"; do
    if [[ -n "$old_tap" && "$old_tap" != "null" ]]; then
      "${ip_cmd[@]}" link delete "$old_tap" >/dev/null 2>&1
    fi
  done

  "${kumabox_cmd[@]}" \
    --root-dir "$root_dir" \
    --run-dir "$run_dir" \
    --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor_path" \
    network teardown --json >/dev/null 2>&1

  "${remove_cmd[@]}" "$root_dir" "$run_dir" "$log_dir"
  mkdir -p "$root_dir" "$run_dir" "$log_dir"
  set -e
}

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
  echo "verify-network-cleanup must run on Linux" >&2
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

clean_previous_state

section "environment checks"
scripts/linux/env-check.sh \
  --kumabox "$kumabox_path" \
  --cloud-hypervisor "$cloud_hypervisor_path" \
  --qemu-img "$qemu_img_path" \
  --strict \
  --network

section "run VM with --network default"
set +e
run_output="$("${kumabox_cmd[@]}" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  run \
  --name "$name" \
  --root-disk "$root_disk" \
  --firmware "$firmware" \
  --network default 2>&1)"
run_status=$?
set -e
if [[ "$run_status" -ne 0 ]]; then
  printf '%s\n' "$run_output" >&2
  vm_exists=1
  print_context
  exit "$run_status"
fi
run_json="$run_output"
vm_exists=1
printf '%s\n' "$run_json"

vm_id="$(printf '%s' "$run_json" | jq -r '.id')"
tap="$(printf '%s' "$run_json" | jq -r '.networkConfigs[0].tap')"
net_id="$(printf '%s' "$run_json" | jq -r '.networkConfigs[0].id')"
guest_ip="$(printf '%s' "$run_json" | jq -r '.networkConfigs[0].network.ip')"
if [[ -z "$vm_id" || "$vm_id" == "null" || -z "$tap" || "$tap" == "null" || -z "$guest_ip" || "$guest_ip" == "null" ]]; then
  echo "run output is missing vm id, tap, or guest ip" >&2
  print_context
  exit 1
fi
printf 'state: vm=%s network=%s tap=%s guest_ip=%s\n' "$vm_id" "$net_id" "$tap" "$guest_ip"

section "network resources after run"
"${ip_cmd[@]}" -d link show dev "$tap"
jq_file '.' "$root_dir/network/index.json"
jq_file '.' "$root_dir/network/leases.json"
jq_file '.' "$root_dir/network/host-tap.json"
if [[ "$(jq_file '.refCount' "$root_dir/network/host-tap.json")" != "1" ]]; then
  echo "host-tap refCount should be 1 after run" >&2
  print_context
  exit 1
fi

section "stop VM"
"${kumabox_cmd[@]}" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  stop "$name" --force

section "network resources after stop"
stopped_json="$("${kumabox_cmd[@]}" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  inspect "$name" --json)"
printf '%s\n' "$stopped_json"
"${ip_cmd[@]}" link show dev "$tap" >/dev/null
network_inspect="$("${kumabox_cmd[@]}" --root-dir "$root_dir" network inspect "$name" --json)"
printf '%s\n' "$network_inspect"
if [[ "$(printf '%s' "$network_inspect" | jq -r '.interfaces | length')" != "1" ]]; then
  echo "network provider record should remain after stop" >&2
  print_context
  exit 1
fi
if ! "${cat_cmd[@]}" "$root_dir/network/leases.json" | jq -e --arg ip "$guest_ip" '.leases[$ip] != null' >/dev/null; then
  echo "IP lease $guest_ip should remain after stop" >&2
  print_context
  exit 1
fi
if [[ "$(jq_file '.refCount' "$root_dir/network/host-tap.json")" != "1" ]]; then
  echo "host-tap refCount should remain 1 after stop" >&2
  print_context
  exit 1
fi
printf 'pass: stop preserved tap=%s lease=%s provider=%s\n' "$tap" "$guest_ip" "$net_id"

section "delete VM"
"${kumabox_cmd[@]}" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  delete "$name" --force
vm_exists=0

section "network resources after delete"
if "${ip_cmd[@]}" link show dev "$tap" >/dev/null 2>&1; then
  echo "tap $tap still exists after delete" >&2
  print_context
  exit 1
fi
jq_file '.' "$root_dir/network/index.json"
jq_file '.' "$root_dir/network/leases.json"
jq_file '.' "$root_dir/network/host-tap.json"
if [[ "$(jq_file '.networks | length' "$root_dir/network/index.json")" != "0" ]]; then
  echo "provider index should be empty after delete" >&2
  print_context
  exit 1
fi
if [[ "$(jq_file '.leases | length' "$root_dir/network/leases.json")" != "0" ]]; then
  echo "IP leases should be empty after delete" >&2
  print_context
  exit 1
fi
if [[ "$(jq_file '.refCount' "$root_dir/network/host-tap.json")" != "0" ]]; then
  echo "host-tap refCount should be 0 after delete" >&2
  print_context
  exit 1
fi
if "${kumabox_cmd[@]}" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  inspect "$name" --json >/dev/null 2>&1; then
  echo "VM record still exists after delete" >&2
  print_context
  exit 1
fi
printf 'pass: delete released tap=%s lease=%s provider=%s and removed VM %s\n' "$tap" "$guest_ip" "$net_id" "$vm_id"

section "teardown global host-tap network"
"${kumabox_cmd[@]}" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  network teardown --json
if "${ip_cmd[@]}" link show dev kumabox0 >/dev/null 2>&1; then
  echo "kumabox0 still exists after teardown" >&2
  print_context
  exit 1
fi

echo "P2-06 network cleanup verification passed"
