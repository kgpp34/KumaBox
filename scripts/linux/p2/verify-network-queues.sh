#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
cloud_hypervisor_path="cloud-hypervisor"
qemu_img_path="qemu-img"
root_dir="/tmp/kumabox-p0/data"
run_dir="/tmp/kumabox-p0/run"
log_dir="/tmp/kumabox-p0/logs"
name_prefix="p2-queues"
root_disk=""
firmware=""
use_sudo=0

usage() {
  cat <<'USAGE'
Usage: scripts/linux/p2/verify-network-queues.sh --root-disk PATH --firmware PATH [options]

Verifies P2-11 queue strategy:
  --cpus 1 => numQueues 2
  --cpus 2 => numQueues 4
  --cpus 4 => numQueues 8

The script also starts, stops, and starts one VM again to prove queue count is
persisted in the VM record and remains stable across restart.

Options:
  --kumabox PATH             kumabox binary path
  --cloud-hypervisor PATH    cloud-hypervisor binary or command
  --qemu-img PATH            qemu-img binary or command
  --root-dir PATH            persistent state directory
  --run-dir PATH             runtime directory
  --log-dir PATH             log directory
  --name-prefix NAME         VM name prefix
  --root-disk PATH           cloud image root disk path
  --firmware PATH            UEFI firmware path
  --sudo                     clean/write state through sudo
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
    --name-prefix)
      require_value "$1" "${2:-}"
      name_prefix="$2"
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
  echo "verify-network-queues must run on Linux" >&2
  exit 1
fi

for bin in jq ip; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "$bin is required" >&2
    exit 1
  fi
done

if [[ "$use_sudo" -eq 1 ]]; then
  kumabox_cmd=(sudo "$kumabox_path")
  remove_cmd=(sudo rm -rf)
  mkdir_cmd=(sudo mkdir -p)
  cat_cmd=(sudo cat)
  ip_cmd=(sudo ip)
else
  kumabox_cmd=("$kumabox_path")
  remove_cmd=(rm -rf)
  mkdir_cmd=(mkdir -p)
  cat_cmd=(cat)
  ip_cmd=(ip)
fi

cleanup_name() {
  local vm_name="$1"
  "${kumabox_cmd[@]}" \
    --root-dir "$root_dir" \
    --run-dir "$run_dir" \
    --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor_path" \
    delete "$vm_name" --force >/dev/null 2>&1 || true
}

cleanup_all() {
  set +e
  cleanup_name "${name_prefix}-1"
  cleanup_name "${name_prefix}-2"
  cleanup_name "${name_prefix}-4"
  "${kumabox_cmd[@]}" \
    --root-dir "$root_dir" \
    --run-dir "$run_dir" \
    --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor_path" \
    network teardown --json >/dev/null 2>&1
  "${ip_cmd[@]}" link delete kumabox0 >/dev/null 2>&1 || true
  "${remove_cmd[@]}" "$root_dir" "$run_dir" "$log_dir"
}
trap cleanup_all EXIT

clean_start() {
  section "clean previous P2-11 state"
  cleanup_all
  "${mkdir_cmd[@]}" "$root_dir" "$run_dir" "$log_dir"
}

verify_created_queue() {
  local cpus="$1"
  local expected="$2"
  local vm_name="${name_prefix}-${cpus}"

  section "create VM with --cpus $cpus"
  local created_json
  created_json="$("${kumabox_cmd[@]}" \
    --root-dir "$root_dir" \
    --run-dir "$run_dir" \
    --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor_path" \
    create \
    --name "$vm_name" \
    --root-disk "$root_disk" \
    --firmware "$firmware" \
    --network default \
    --cpus "$cpus")"
  printf '%s\n' "$created_json"

  local config_path
  config_path="$(printf '%s' "$created_json" | jq -r '.config')"
  if [[ "$(printf '%s' "$created_json" | jq -r '.cpus')" != "$cpus" ]]; then
    echo "VM record cpus should be $cpus" >&2
    exit 1
  fi
  if [[ "$(printf '%s' "$created_json" | jq -r '.networkConfigs[0].numQueues')" != "$expected" ]]; then
    echo "network config numQueues should be $expected" >&2
    exit 1
  fi

  section "rendered queue config for --cpus $cpus"
  "${cat_cmd[@]}" "$config_path" | jq '{cpus, nets, args}'
  if [[ "$("${cat_cmd[@]}" "$config_path" | jq -r '.cpus.boot')" != "$cpus" ]]; then
    echo "rendered cpus.boot should be $cpus" >&2
    exit 1
  fi
  if [[ "$("${cat_cmd[@]}" "$config_path" | jq -r '.nets[0].numQueues')" != "$expected" ]]; then
    echo "rendered net numQueues should be $expected" >&2
    exit 1
  fi
  if ! "${cat_cmd[@]}" "$config_path" | jq -e '.args as $args | any(range(0; ($args|length)-1); $args[.] == "--cpus" and $args[.+1] == "boot='"$cpus"'")' >/dev/null; then
    echo "rendered Cloud Hypervisor args missing --cpus boot=$cpus" >&2
    exit 1
  fi
  if ! "${cat_cmd[@]}" "$config_path" | jq -e '.args[] | select(contains("num_queues='"$expected"',queue_size=256"))' >/dev/null; then
    echo "rendered Cloud Hypervisor args missing num_queues=$expected" >&2
    exit 1
  fi
}

verify_restart_stability() {
  local vm_name="${name_prefix}-2"
  section "restart stability for --cpus 2"
  "${kumabox_cmd[@]}" \
    --root-dir "$root_dir" \
    --run-dir "$run_dir" \
    --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor_path" \
    start "$vm_name" >/dev/null
  "${kumabox_cmd[@]}" \
    --root-dir "$root_dir" \
    --run-dir "$run_dir" \
    --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor_path" \
    stop "$vm_name" --timeout 10s --force >/dev/null
  local restarted_json
  restarted_json="$("${kumabox_cmd[@]}" \
    --root-dir "$root_dir" \
    --run-dir "$run_dir" \
    --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor_path" \
    start "$vm_name")"
  printf '%s\n' "$restarted_json"
  local config_path
  config_path="$(printf '%s' "$restarted_json" | jq -r '.config')"
  if [[ "$(printf '%s' "$restarted_json" | jq -r '.networkConfigs[0].numQueues')" != "4" ]]; then
    echo "restarted VM record numQueues should remain 4" >&2
    exit 1
  fi
  if [[ "$("${cat_cmd[@]}" "$config_path" | jq -r '.nets[0].numQueues')" != "4" ]]; then
    echo "restarted rendered numQueues should remain 4" >&2
    exit 1
  fi
  cleanup_name "$vm_name"
}

clean_start

section "environment checks"
scripts/linux/env-check.sh \
  --kumabox "$kumabox_path" \
  --cloud-hypervisor "$cloud_hypervisor_path" \
  --qemu-img "$qemu_img_path" \
  --strict \
  --network

verify_created_queue 1 2
verify_created_queue 2 4
verify_created_queue 4 8
verify_restart_stability

echo "P2-11 network queue strategy verification passed"
