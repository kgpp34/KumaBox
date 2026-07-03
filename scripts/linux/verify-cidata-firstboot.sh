#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
root_dir="/tmp/kumabox-p0/data"
run_dir="/tmp/kumabox-p0/run"
log_dir="/tmp/kumabox-p0/logs"
cloud_hypervisor_path="cloud-hypervisor"
name="p1-cidata"
root_disk=""
firmware=""

usage() {
  cat <<'USAGE'
Usage: scripts/linux/verify-cidata-firstboot.sh --root-disk PATH --firmware PATH [options]

Options:
  --kumabox PATH             kumabox binary path, defaults to ./bin/kumabox
  --cloud-hypervisor PATH    cloud-hypervisor binary path, defaults to cloud-hypervisor
  --root-dir PATH            store directory, defaults to /tmp/kumabox-p0/data
  --run-dir PATH             runtime directory, defaults to /tmp/kumabox-p0/run
  --log-dir PATH             log directory, defaults to /tmp/kumabox-p0/logs
  --name NAME                VM name, defaults to p1-cidata
  --root-disk PATH           cloud image root disk path
  --firmware PATH            UEFI firmware path for cloud-image boot

Verifies P1-04 cidata first-boot behavior:
clean data/run/logs -> create -> cidata attached -> start -> firstBooted=true
-> stop -> second start -> cidata skipped -> delete VM -> clean data/run/logs.
The fixtures directory is never removed.
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
  echo "verify-cidata-firstboot must run inside the Linux VM" >&2
  exit 1
fi

for bin in jq; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "$bin is required" >&2
    exit 1
  fi
done

for path in "$kumabox_path" "$root_disk" "$firmware"; do
  if [[ ! -e "$path" ]]; then
    echo "required path does not exist: $path" >&2
    exit 1
  fi
done

if [[ ! -x "$kumabox_path" ]]; then
  echo "kumabox is not executable: $kumabox_path" >&2
  exit 1
fi

vm_exists=0
cleanup() {
  set +e
  if [[ "$vm_exists" -eq 1 ]]; then
    "$kumabox_path" \
      --root-dir "$root_dir" \
      --run-dir "$run_dir" \
      --log-dir "$log_dir" \
      --cloud-hypervisor-bin "$cloud_hypervisor_path" \
      delete "$name" --force >/dev/null 2>&1
  fi
  rm -rf "$root_dir" "$run_dir" "$log_dir"
}
trap cleanup EXIT

rm -rf "$root_dir" "$run_dir" "$log_dir"
mkdir -p "$root_dir" "$run_dir" "$log_dir"

scripts/linux/env-check.sh \
  --kumabox "$kumabox_path" \
  --cloud-hypervisor "$cloud_hypervisor_path" \
  --strict

created_json="$("$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  create \
  --name "$name" \
  --root-disk "$root_disk" \
  --firmware "$firmware")"
vm_exists=1
printf '%s\n' "$created_json"

vm_id="$(printf '%s' "$created_json" | jq -r '.id')"
config_path="$(printf '%s' "$created_json" | jq -r '.config')"
cidata_disk="$(printf '%s' "$created_json" | jq -r '.metadata.cidataDisk')"

if [[ -z "$vm_id" || "$vm_id" == "null" ]]; then
  echo "could not parse VM id from create output" >&2
  exit 1
fi
if [[ -z "$config_path" || "$config_path" == "null" || ! -f "$config_path" ]]; then
  echo "rendered Cloud Hypervisor config is missing: $config_path" >&2
  exit 1
fi
if [[ -z "$cidata_disk" || "$cidata_disk" == "null" || ! -f "$cidata_disk" ]]; then
  echo "cidata disk is missing after create: $cidata_disk" >&2
  exit 1
fi

if [[ "$(jq '.firstBooted // false' <<<"$created_json")" != "false" ]]; then
  echo "new VM should not be firstBooted" >&2
  exit 1
fi

first_disk_count="$(jq '[.disks[] | select(.path == "'"$cidata_disk"'")] | length' "$config_path")"
if [[ "$first_disk_count" != "1" ]]; then
  echo "cidata disk is not attached before first boot" >&2
  jq '.disks' "$config_path" >&2
  exit 1
fi
echo "pass: cidata attached before first boot"

start_json="$("$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  start "$name")"
printf '%s\n' "$start_json"

if [[ "$(printf '%s' "$start_json" | jq -r '.state')" != "running" ]]; then
  echo "VM did not enter running state after first start" >&2
  exit 1
fi
if [[ "$(printf '%s' "$start_json" | jq '.firstBooted // false')" != "true" ]]; then
  echo "VM was not marked firstBooted after first start" >&2
  exit 1
fi
echo "pass: first start marked firstBooted"

stop_json="$("$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  stop "$name" --force)"
printf '%s\n' "$stop_json"

if [[ "$(printf '%s' "$stop_json" | jq -r '.state')" != "stopped" ]]; then
  echo "VM did not enter stopped state" >&2
  exit 1
fi

second_start_json="$("$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  start "$name")"
printf '%s\n' "$second_start_json"

if [[ "$(printf '%s' "$second_start_json" | jq -r '.state')" != "running" ]]; then
  echo "VM did not enter running state after second start" >&2
  exit 1
fi

second_disk_count="$(jq '[.disks[] | select(.path == "'"$cidata_disk"'")] | length' "$config_path")"
second_arg_count="$(jq '[.args[] | select(contains("'"$cidata_disk"'"))] | length' "$config_path")"
if [[ "$second_disk_count" != "0" || "$second_arg_count" != "0" ]]; then
  echo "cidata disk is still attached after first boot" >&2
  jq '.disks' "$config_path" >&2
  jq '.args' "$config_path" >&2
  exit 1
fi
echo "pass: cidata skipped after first boot"

delete_json="$("$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  delete "$name" --force)"
vm_exists=0
printf '%s\n' "$delete_json"

if [[ -e "$run_dir/vms/$vm_id" ]]; then
  echo "VM run directory still exists after delete: $run_dir/vms/$vm_id" >&2
  exit 1
fi

echo "P1-04 cidata first-boot verification passed"
