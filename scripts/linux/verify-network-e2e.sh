#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
cloud_hypervisor_path="cloud-hypervisor"
qemu_img_path="qemu-img"
root_dir="/tmp/kumabox-p0/data"
run_dir="/tmp/kumabox-p0/run"
log_dir="/tmp/kumabox-p0/logs"
name="p2-e2e"
root_disk=""
firmware=""
use_sudo=0
timeout=240
ping_interval=5
tap=""
vm_exists=0
script_status=1

usage() {
  cat <<'USAGE'
Usage: scripts/linux/verify-network-e2e.sh --root-disk PATH --firmware PATH [options]

Starts a real microVM with --network default and verifies host-to-guest
connectivity by pinging the guest static IP assigned by KumaBox. This is the
P2 network e2e smoke test before guest-agent/exec exists.

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
  --timeout SECONDS          max seconds to wait for guest ping, default 240
  --ping-interval SECONDS    seconds between ping attempts, default 5
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

print_failure_context() {
  set +e
  section "failure context: VM inspect"
  "${kumabox_cmd[@]}" \
    --root-dir "$root_dir" \
    --run-dir "$run_dir" \
    --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor_path" \
    inspect "$name" --json 2>/dev/null || true

  section "failure context: network inspect"
  "${kumabox_cmd[@]}" \
    --root-dir "$root_dir" \
    network inspect "$name" --json 2>/dev/null || true

  if [[ -n "$tap" && "$tap" != "null" ]]; then
    section "failure context: host tap"
    "${ip_cmd[@]}" -d link show dev "$tap" 2>/dev/null || true
  fi

  section "failure context: bridge"
  "${ip_cmd[@]}" -d link show dev kumabox0 2>/dev/null || true
  "${ip_cmd[@]}" -4 addr show dev kumabox0 2>/dev/null || true

  if [[ -n "${console_log:-}" && "$console_log" != "null" ]]; then
    section "failure context: console tail"
    "${cat_cmd[@]}" "$console_log" 2>/dev/null | tail -n 120 || true
  fi

  vm_run_dir=""
  if [[ -d "$run_dir/vms" ]]; then
    vm_run_dir="$("${find_cmd[@]}" "$run_dir/vms" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | sort | tail -n 1)"
  fi
  if [[ -n "$vm_run_dir" ]]; then
    section "failure context: rendered config"
    "${cat_cmd[@]}" "$vm_run_dir/cloud-hypervisor.json" 2>/dev/null | jq '.' || true

    section "failure context: cloud-hypervisor stdout"
    "${cat_cmd[@]}" "$log_dir/vms/$(basename "$vm_run_dir")/cloud-hypervisor.stdout.log" 2>/dev/null | tail -n 120 || true

    section "failure context: cloud-hypervisor stderr"
    "${cat_cmd[@]}" "$log_dir/vms/$(basename "$vm_run_dir")/cloud-hypervisor.stderr.log" 2>/dev/null | tail -n 120 || true
  fi
}

clean_previous_state() {
  section "clean previous P2 network e2e state"
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

  "${kumabox_cmd[@]}" \
    --root-dir "$root_dir" \
    --run-dir "$run_dir" \
    --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor_path" \
    network teardown --json >/dev/null 2>&1

  for old_tap in "${old_taps[@]}"; do
    if [[ -n "$old_tap" && "$old_tap" != "null" ]]; then
      "${ip_cmd[@]}" link delete "$old_tap" >/dev/null 2>&1
    fi
  done

  "${remove_cmd[@]}" "$root_dir" "$run_dir" "$log_dir"
  mkdir -p "$root_dir" "$run_dir" "$log_dir"
  set -e
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
    --timeout)
      require_value "$1" "${2:-}"
      timeout="$2"
      shift 2
      ;;
    --ping-interval)
      require_value "$1" "${2:-}"
      ping_interval="$2"
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
  echo "verify-network-e2e must run on Linux" >&2
  exit 1
fi

for bin in jq ping; do
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
  find_cmd=(sudo find)
else
  kumabox_cmd=("$kumabox_path")
  remove_cmd=(rm -rf)
  ip_cmd=(ip)
  cat_cmd=(cat)
  find_cmd=(find)
fi

cleanup() {
  set +e
  if [[ "$script_status" -ne 0 ]]; then
    printf '\n==> preserving failed P2 network e2e state\n' >&2
    printf 'state: preserved root_dir=%s run_dir=%s log_dir=%s\n' "$root_dir" "$run_dir" "$log_dir" >&2
    printf 'state: rerun this script to clean preserved state before the next attempt\n' >&2
    return
  fi
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
  echo "kumabox run failed before VM reached running state" >&2
  print_failure_context
  exit "$run_status"
fi
run_json="$run_output"
vm_exists=1
printf '%s\n' "$run_json"

state="$(printf '%s' "$run_json" | jq -r '.state')"
vm_id="$(printf '%s' "$run_json" | jq -r '.id')"
tap="$(printf '%s' "$run_json" | jq -r '.networkConfigs[0].tap')"
guest_ip="$(printf '%s' "$run_json" | jq -r '.networkConfigs[0].network.ip')"
console_log="$(printf '%s' "$run_json" | jq -r '.logDir + "/console.log"')"
config_path="$(printf '%s' "$run_json" | jq -r '.config')"

if [[ "$state" != "running" ]]; then
  echo "VM did not enter running state: $state" >&2
  print_failure_context
  exit 1
fi
if [[ -z "$vm_id" || "$vm_id" == "null" || -z "$tap" || "$tap" == "null" || -z "$guest_ip" || "$guest_ip" == "null" ]]; then
  echo "run output is missing vm id, tap, or guest ip" >&2
  print_failure_context
  exit 1
fi
printf 'state: vm=%s tap=%s guest_ip=%s console=%s\n' "$vm_id" "$tap" "$guest_ip" "$console_log"

section "network inspect"
network_inspect="$("${kumabox_cmd[@]}" \
  --root-dir "$root_dir" \
  network inspect "$name" --json)"
printf '%s\n' "$network_inspect"
if [[ "$(printf '%s' "$network_inspect" | jq -r '.drift | length')" != "0" ]]; then
  echo "network inspect reported drift" >&2
  exit 1
fi

section "host tap link"
"${ip_cmd[@]}" link show dev "$tap" >/dev/null
tap_detail="$("${ip_cmd[@]}" -d link show dev "$tap")"
printf '%s\n' "$tap_detail"
if [[ "$tap_detail" != *"pi off"* || "$tap_detail" != *"vnet_hdr on"* ]]; then
  echo "tap $tap must be no-pi with vnet_hdr on" >&2
  print_failure_context
  exit 1
fi
master="$(basename "$(readlink "/sys/class/net/$tap/master")")"
if [[ "$master" != "kumabox0" ]]; then
  echo "tap $tap master = $master, want kumabox0" >&2
  print_failure_context
  exit 1
fi
printf 'state: tap %s master=%s\n' "$tap" "$master"

section "bridge state"
"${ip_cmd[@]}" -d link show dev kumabox0
"${ip_cmd[@]}" -4 addr show dev kumabox0

section "cloud-hypervisor net config"
jq_file '.nets' "$config_path"

section "wait for host-to-guest ping"
deadline=$((SECONDS + timeout))
wait_start=$SECONDS
attempt=0
printf 'state: waiting up to %ss for guest boot and cloud-init network config\n' "$timeout"
until ping -c 1 -W 2 "$guest_ip" >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  if (( SECONDS >= deadline )); then
    echo "guest IP $guest_ip did not respond to ping within ${timeout}s" >&2
    print_failure_context
    exit 1
  fi
  elapsed=$((SECONDS - wait_start))
  printf 'state: ping attempt %d failed after %ss/%ss; waiting %ss for guest network\n' "$attempt" "$elapsed" "$timeout" "$ping_interval"
  if (( attempt % 6 == 0 )); then
    section "guest console tail while waiting"
    "${cat_cmd[@]}" "$console_log" 2>/dev/null | tail -n 40 || true
    section "wait for host-to-guest ping"
  fi
  sleep "$ping_interval"
done
printf 'pass: host can ping guest %s\n' "$guest_ip"

section "console tail"
"${cat_cmd[@]}" "$console_log" 2>/dev/null | tail -n 80 || true

section "stop and delete VM"
"${kumabox_cmd[@]}" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  delete "$name" --force
vm_exists=0

if "${ip_cmd[@]}" link show dev "$tap" >/dev/null 2>&1; then
  printf 'state: P2-06 cleanup is not implemented yet; removing leftover tap %s manually\n' "$tap"
  "${ip_cmd[@]}" link delete "$tap"
fi

echo "P2 network e2e verification passed"
script_status=0
