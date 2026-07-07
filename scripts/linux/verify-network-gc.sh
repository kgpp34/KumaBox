#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
cloud_hypervisor_path="cloud-hypervisor"
qemu_img_path="qemu-img"
root_dir="/tmp/kumabox-p0/data"
run_dir="/tmp/kumabox-p0/run"
log_dir="/tmp/kumabox-p0/logs"
name="p2-gc"
root_disk=""
firmware=""
use_sudo=0

usage() {
  cat <<'USAGE'
Usage: scripts/linux/verify-network-gc.sh --root-disk PATH --firmware PATH [options]

Verifies P2-07 network GC dry-run behavior. The script builds synthetic network
state under the KumaBox root: one active VM network, one pending cleanup record,
and one orphan lease. It then verifies gc --dry-run reports only the network
cleanup candidates and fails closed on corrupt network leases.

Options:
  --kumabox PATH             kumabox binary path
  --cloud-hypervisor PATH    cloud-hypervisor binary or command
  --qemu-img PATH            qemu-img binary or command
  --root-dir PATH            persistent state directory
  --run-dir PATH             runtime directory
  --log-dir PATH             log directory
  --name NAME                VM name
  --root-disk PATH           root disk fixture path
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

write_file() {
  local path="$1"
  local content="$2"
  local tmp
  tmp="$(mktemp)"
  printf '%s\n' "$content" >"$tmp"
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
  echo "verify-network-gc must run on Linux" >&2
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
  mkdir_cmd=(sudo mkdir -p)
  cat_cmd=(sudo cat)
  copy_cmd=(sudo cp)
else
  kumabox_cmd=("$kumabox_path")
  remove_cmd=(rm -rf)
  mkdir_cmd=(mkdir -p)
  cat_cmd=(cat)
  copy_cmd=(cp)
fi

section "clean previous P2-07 state"
"${remove_cmd[@]}" "$root_dir" "$run_dir" "$log_dir"
"${mkdir_cmd[@]}" "$root_dir" "$run_dir" "$log_dir"

section "environment checks"
scripts/linux/env-check.sh \
  --kumabox "$kumabox_path" \
  --cloud-hypervisor "$cloud_hypervisor_path" \
  --qemu-img "$qemu_img_path" \
  --strict

section "create active VM record"
created_json="$("${kumabox_cmd[@]}" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  create \
  --name "$name" \
  --root-disk "$root_disk" \
  --firmware "$firmware" \
  --network none)"
printf '%s\n' "$created_json"
vm_id="$(printf '%s' "$created_json" | jq -r '.id')"

active_net_id="net_active00000000"
active_tap="kbtapactive0"
active_mac="5a:00:00:00:00:11"
active_ip="10.88.0.2"
pending_net_id="net_pending000000"
pending_tap="kbtappending0"
orphan_ip="10.88.0.77"
network_dir="$root_dir/network"
vm_index="$root_dir/backends/cloud-hypervisor/index.json"

section "write synthetic network provider state"
"${mkdir_cmd[@]}" "$network_dir"
write_file "$network_dir/index.json" "{
  \"schemaVersion\": \"kumabox.network.index.v1\",
  \"networks\": {
    \"$active_net_id\": {
      \"id\": \"$active_net_id\",
      \"vmId\": \"$vm_id\",
      \"network\": \"default\",
      \"provider\": \"host-tap\",
      \"ifName\": \"eth0\",
      \"tap\": \"$active_tap\",
      \"mac\": \"$active_mac\",
      \"numQueues\": 2,
      \"queueSize\": 256,
      \"bridgeDev\": \"kumabox0\",
      \"ips\": [\"$active_ip/16\"],
      \"gateway\": \"10.88.0.1\",
      \"dns\": [\"1.1.1.1\", \"8.8.8.8\"],
      \"cleanup\": {\"pending\": false},
      \"createdAt\": \"2026-07-07T00:00:00Z\",
      \"updatedAt\": \"2026-07-07T00:00:00Z\"
    },
    \"$pending_net_id\": {
      \"id\": \"$pending_net_id\",
      \"vmId\": \"kb_missing\",
      \"network\": \"default\",
      \"provider\": \"host-tap\",
      \"ifName\": \"eth0\",
      \"tap\": \"$pending_tap\",
      \"mac\": \"5a:00:00:00:00:22\",
      \"numQueues\": 2,
      \"queueSize\": 256,
      \"bridgeDev\": \"kumabox0\",
      \"ips\": [\"10.88.0.42/16\"],
      \"gateway\": \"10.88.0.1\",
      \"dns\": [\"1.1.1.1\", \"8.8.8.8\"],
      \"cleanup\": {
        \"pending\": true,
        \"reason\": \"tap delete failed\",
        \"lastAttemptAt\": \"2026-07-07T00:00:01Z\"
      },
      \"createdAt\": \"2026-07-07T00:00:00Z\",
      \"updatedAt\": \"2026-07-07T00:00:01Z\"
    }
  }
}"
write_file "$network_dir/leases.json" "{
  \"schemaVersion\": \"kumabox.network.leases.v1\",
  \"cidr\": \"10.88.0.0/16\",
  \"leases\": {
    \"$active_ip\": {
      \"vmId\": \"$vm_id\",
      \"mac\": \"$active_mac\",
      \"tap\": \"$active_tap\",
      \"createdAt\": \"2026-07-07T00:00:00Z\"
    },
    \"$orphan_ip\": {
      \"vmId\": \"kb_orphan\",
      \"mac\": \"5a:00:00:00:00:33\",
      \"tap\": \"kbtaporphan0\",
      \"createdAt\": \"2026-07-07T00:00:00Z\"
    }
  }
}"

section "attach active network config to VM record"
tmp_vm="$(mktemp)"
"${cat_cmd[@]}" "$vm_index" | jq \
  --arg vm_id "$vm_id" \
  --arg net_id "$active_net_id" \
  --arg tap "$active_tap" \
  --arg mac "$active_mac" \
  --arg ip "$active_ip" \
  '.vms[$vm_id].network = "default"
   | .vms[$vm_id].networkConfigs = [{
      id: $net_id,
      tap: $tap,
      mac: $mac,
      numQueues: 2,
      queueSize: 256,
      backend: "host-tap",
      bridgeDev: "kumabox0",
      network: {
        ip: $ip,
        gateway: "10.88.0.1",
        prefix: 16,
        dns: ["1.1.1.1", "8.8.8.8"]
      }
    }]' >"$tmp_vm"
"${copy_cmd[@]}" "$tmp_vm" "$vm_index"
rm -f "$tmp_vm"

section "provider index"
"${cat_cmd[@]}" "$network_dir/index.json" | jq '.'
section "leases"
"${cat_cmd[@]}" "$network_dir/leases.json" | jq '.'

section "gc dry-run"
gc_json="$("${kumabox_cmd[@]}" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  gc --dry-run --json)"
printf '%s\n' "$gc_json"

if printf '%s' "$gc_json" | jq -e --arg path "$active_tap" '.candidates[]? | select(.path == $path)' >/dev/null; then
  echo "active VM tap was reported as a GC candidate" >&2
  exit 1
fi
if ! printf '%s' "$gc_json" | jq -e --arg path "$pending_net_id" '.candidates[]? | select(.component == "network" and .type == "pending_cleanup" and .path == $path)' >/dev/null; then
  echo "pending cleanup candidate missing" >&2
  exit 1
fi
if ! printf '%s' "$gc_json" | jq -e --arg path "$pending_tap" '.candidates[]? | select(.component == "network" and .type == "stale_tap" and .path == $path)' >/dev/null; then
  echo "stale tap candidate missing" >&2
  exit 1
fi
if ! printf '%s' "$gc_json" | jq -e --arg path "$orphan_ip" '.candidates[]? | select(.component == "network" and .type == "orphan_lease" and .path == $path)' >/dev/null; then
  echo "orphan lease candidate missing" >&2
  exit 1
fi

section "gc fail-closed on corrupt leases"
write_file "$network_dir/leases.json" "{"
set +e
"${kumabox_cmd[@]}" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  gc --dry-run --json >/tmp/kumabox-p2-gc-corrupt.out 2>&1
corrupt_status=$?
set -e
cat /tmp/kumabox-p2-gc-corrupt.out
rm -f /tmp/kumabox-p2-gc-corrupt.out
if [[ "$corrupt_status" -eq 0 ]]; then
  echo "gc should fail when network leases are corrupt" >&2
  exit 1
fi

section "cleanup"
"${remove_cmd[@]}" "$root_dir" "$run_dir" "$log_dir"

echo "P2-07 network GC verification passed"
