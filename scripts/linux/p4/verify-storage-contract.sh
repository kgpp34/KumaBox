#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
root_dir="/tmp/kumabox-p0/data"
run_dir="/tmp/kumabox-p0/run"
log_dir="/tmp/kumabox-p0/logs"
name="p4-storage-contract"
image=""

usage() {
  cat <<'USAGE'
Usage: scripts/linux/p4/verify-storage-contract.sh [options]

Options:
  --kumabox PATH   kumabox binary path, defaults to ./bin/kumabox
  --root-dir PATH  durable state directory, defaults to /tmp/kumabox-p0/data
  --run-dir PATH   runtime directory, defaults to /tmp/kumabox-p0/run
  --log-dir PATH   log directory, defaults to /tmp/kumabox-p0/logs
  --name NAME      verification name prefix, defaults to p4-storage-contract
  --image REF      optional managed OCI image for inspecting the new disk contract

Verifies storage contract validation and daemonless per-VM operation locks.
The script preserves /tmp/kumabox-p0/fixtures and unrelated KumaBox resources.
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
    --root-dir) require_value "$1" "${2:-}"; root_dir="$2"; shift 2 ;;
    --run-dir) require_value "$1" "${2:-}"; run_dir="$2"; shift 2 ;;
    --log-dir) require_value "$1" "${2:-}"; log_dir="$2"; shift 2 ;;
    --name) require_value "$1" "${2:-}"; name="$2"; shift 2 ;;
    --image) require_value "$1" "${2:-}"; image="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "storage contract verification must run on Linux" >&2
  exit 1
fi
for command in jq flock timeout go; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "$command is required for storage contract verification" >&2
    exit 1
  fi
done
if [[ ! -x "$kumabox_path" ]]; then
  echo "kumabox is not executable: $kumabox_path" >&2
  exit 1
fi

mkdir -p "$root_dir" "$run_dir" "$log_dir"
fixture_dir="$run_dir/p4-storage-contract-fixtures"
mkdir -p "$fixture_dir"
touch "$fixture_dir/root.raw" "$fixture_dir/vmlinuz" "$fixture_dir/initrd.img"

name_a="${name}-a"
name_b="${name}-b"
name_oci="${name}-oci"

kb() {
  "$kumabox_path" \
    --root-dir "$root_dir" \
    --run-dir "$run_dir" \
    --log-dir "$log_dir" \
    "$@"
}

cleanup() {
  local exit_code=$?
  kb delete "$name_a" --force >/dev/null 2>&1 || true
  kb delete "$name_b" --force >/dev/null 2>&1 || true
  kb delete "$name_oci" --force >/dev/null 2>&1 || true
  rm -rf "$fixture_dir"
  return "$exit_code"
}
trap cleanup EXIT

step "run storage contract unit specifications"
go test ./internal/vmstore -run 'TestValidateStorageContract|TestStoreReadsLegacyP3StorageRecord|TestCreatePlacesCOWInDurableOwnerDirectory' -count=1

step "clean previous verification VM records"
kb delete "$name_a" --force >/dev/null 2>&1 || true
kb delete "$name_b" --force >/dev/null 2>&1 || true
kb delete "$name_oci" --force >/dev/null 2>&1 || true

create_vm() {
  local vm_name="$1"
  kb create \
    --name "$vm_name" \
    --root-disk "$fixture_dir/root.raw" \
    --kernel "$fixture_dir/vmlinuz" \
    --initrd "$fixture_dir/initrd.img" \
    --network none
}

step "create two independent VM records"
vm_a_json="$(create_vm "$name_a")"
vm_b_json="$(create_vm "$name_b")"
printf '%s\n' "$vm_a_json"
printf '%s\n' "$vm_b_json"
vm_a_id="$(printf '%s' "$vm_a_json" | jq -r '.id')"
vm_b_id="$(printf '%s' "$vm_b_json" | jq -r '.id')"
printf 'state: vm_a=%s vm_b=%s\n' "$vm_a_id" "$vm_b_id"

lock_dir="$root_dir/locks/vms"
lock_a="$lock_dir/$vm_a_id.lock"
mkdir -p "$lock_dir"

step "hold VM A operation lock from another process"
(
  flock -x 9
  printf 'state: external process holds %s\n' "$lock_a"
  sleep 300
) 9>"$lock_a" &
holder_pid=$!
for _ in $(seq 1 50); do
  if ! flock -n "$lock_a" true 2>/dev/null; then
    break
  fi
  sleep 0.02
done
if flock -n "$lock_a" true 2>/dev/null; then
  echo "failed to establish external VM lock" >&2
  kill "$holder_pid" >/dev/null 2>&1 || true
  exit 1
fi

step "same VM mutation must wait for its operation lock"
set +e
same_output="$(timeout 1s "$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  start "$name_a" 2>&1)"
same_status=$?
set -e
printf '%s\n' "$same_output"
if [[ "$same_status" -ne 124 ]]; then
  echo "same VM start did not block on the operation lock: status=$same_status" >&2
  kill "$holder_pid" >/dev/null 2>&1 || true
  exit 1
fi
printf 'pass: same VM mutation remained serialized for 1s\n'

step "VM B mutation must not wait for VM A lock"
set +e
other_output="$(timeout 3s "$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin /bin/false \
  start "$name_b" 2>&1)"
other_status=$?
set -e
printf '%s\n' "$other_output"
if [[ "$other_status" -eq 124 ]]; then
  echo "VM B was incorrectly blocked by VM A operation lock" >&2
  kill "$holder_pid" >/dev/null 2>&1 || true
  exit 1
fi
printf 'pass: VM B reached its backend independently (status=%s)\n' "$other_status"

step "terminate lock owner and verify automatic flock release"
kill -9 "$holder_pid" >/dev/null 2>&1 || true
wait "$holder_pid" >/dev/null 2>&1 || true
if ! flock -n "$lock_a" true; then
  echo "VM operation lock remained held after owner exit" >&2
  exit 1
fi
printf 'pass: lock released automatically after owner exit\n'

if [[ -n "$image" ]]; then
  step "create managed OCI VM and inspect the new storage contract"
  oci_json="$(kb create "$image" --name "$name_oci" --network none --storage 64M)"
  printf '%s\n' "$oci_json" | jq '{id, image, storageConfigs}'
  cow_path="$(printf '%s' "$oci_json" | jq -r '.storageConfigs[] | select(.role == "cow") | .path')"
  if [[ "$cow_path" != "$root_dir/storage/vms/"*"/cow.ext4" ]]; then
    echo "OCI COW path is not in durable VM storage: $cow_path" >&2
    exit 1
  fi
  printf '%s' "$oci_json" | jq -e '
    any(.storageConfigs[]; .role == "layer" and .readonly == true and .format == "raw" and .filesystem == "erofs") and
    any(.storageConfigs[]; .role == "cow" and .readonly == false and .format == "raw" and .filesystem == "ext4" and .base.family == "oci" and (.base.digest | length > 0))
  ' >/dev/null
  printf 'pass: OCI storage contract and durable COW owner path are valid\n'
else
  step "managed OCI contract inspection skipped"
  printf 'state: pass --image REF to inspect a real P3 managed OCI image record\n'
fi

step "cleanup verification VM records"
kb delete "$name_a" --force >/dev/null
kb delete "$name_b" --force >/dev/null
if [[ -n "$image" ]]; then
  kb delete "$name_oci" --force >/dev/null
fi

echo "P4 storage contract and VM operation lock verification passed"
