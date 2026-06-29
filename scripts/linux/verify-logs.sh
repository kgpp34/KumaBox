#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
root_dir="/tmp/kumabox-p0/data"
run_dir="/tmp/kumabox-p0/run"
log_dir="/tmp/kumabox-p0/logs"
cloud_hypervisor_path="cloud-hypervisor"
name="p0-logs"
tail_lines="50"
root_disk=""
kernel=""
initrd=""
firmware=""

usage() {
  cat <<'USAGE'
Usage:
  scripts/linux/verify-logs.sh --root-disk PATH --firmware PATH [options]
  scripts/linux/verify-logs.sh --root-disk PATH --kernel PATH --initrd PATH [options]

Options:
  --kumabox PATH             kumabox binary path, defaults to ./bin/kumabox
  --cloud-hypervisor PATH    cloud-hypervisor binary path, defaults to cloud-hypervisor
  --root-dir PATH            store directory, defaults to /tmp/kumabox-p0/data
  --run-dir PATH             runtime directory, defaults to /tmp/kumabox-p0/run
  --log-dir PATH             log directory, defaults to /tmp/kumabox-p0/logs
  --name NAME                VM name, defaults to p0-logs
  --tail N                   number of log lines, defaults to 50
  --firmware PATH            UEFI firmware path for cloud-image boot

Runs the P0-08 logs path inside a Linux VM with KVM:
run VM -> logs VM --source stdout/stderr --tail N -> output must include VMM logs.
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

cleanup_vm() {
  "$kumabox_path" \
    --root-dir "$root_dir" \
    --run-dir "$run_dir" \
    --log-dir "$log_dir" \
    stop "$name" --force >/dev/null 2>&1 || true
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
    --tail)
      require_value "$1" "${2:-}"
      tail_lines="$2"
      shift 2
      ;;
    --root-disk)
      require_value "$1" "${2:-}"
      root_disk="$2"
      shift 2
      ;;
    --kernel)
      require_value "$1" "${2:-}"
      kernel="$2"
      shift 2
      ;;
    --initrd)
      require_value "$1" "${2:-}"
      initrd="$2"
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

if [[ -z "$root_disk" ]]; then
  usage >&2
  exit 2
fi

if [[ -n "$firmware" && ( -n "$kernel" || -n "$initrd" ) ]]; then
  echo "--firmware cannot be combined with --kernel or --initrd" >&2
  exit 2
fi

if [[ -z "$firmware" && ( -z "$kernel" || -z "$initrd" ) ]]; then
  usage >&2
  exit 2
fi

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "verify-logs must run inside the Linux VM" >&2
  exit 1
fi

required_paths=("$kumabox_path" "$root_disk")
if [[ -n "$firmware" ]]; then
  required_paths+=("$firmware")
else
  required_paths+=("$kernel" "$initrd")
fi

for path in "${required_paths[@]}"; do
  if [[ ! -e "$path" ]]; then
    echo "required path does not exist: $path" >&2
    exit 1
  fi
done

if [[ ! -x "$kumabox_path" ]]; then
  echo "kumabox is not executable: $kumabox_path" >&2
  exit 1
fi

scripts/linux/env-check.sh \
  --kumabox "$kumabox_path" \
  --cloud-hypervisor "$cloud_hypervisor_path" \
  --strict

run_args=(
  "$kumabox_path"
  --root-dir "$root_dir"
  --run-dir "$run_dir"
  --log-dir "$log_dir"
  --cloud-hypervisor-bin "$cloud_hypervisor_path"
  run
  --name "$name"
  --root-disk "$root_disk"
)
if [[ -n "$firmware" ]]; then
  run_args+=(--firmware "$firmware")
else
  run_args+=(--kernel "$kernel" --initrd "$initrd")
fi

"${run_args[@]}" >/dev/null
trap cleanup_vm EXIT

inspect_json="$("$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  inspect "$name" --json)"

printf '%s\n' "$inspect_json"

if ! printf '%s' "$inspect_json" | grep -q '"state": "running"'; then
  echo "VM did not enter running state" >&2
  exit 1
fi

stdout_logs="$("$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  logs "$name" --source stdout --tail "$tail_lines")"

stderr_logs="$("$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  logs "$name" --source stderr --tail "$tail_lines")"

printf '%s\n' "$stdout_logs"
printf '%s\n' "$stderr_logs"

if [[ -z "$stdout_logs" ]]; then
  echo "stdout logs output is empty" >&2
  exit 1
fi

if [[ -z "$stderr_logs" ]]; then
  echo "stderr logs output is empty" >&2
  exit 1
fi

missing_output="$("$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  logs "missing-$name" --tail "$tail_lines" 2>&1 || true)"

if ! printf '%s' "$missing_output" | grep -qi 'not found'; then
  echo "logs for a missing VM did not return a not found error" >&2
  exit 1
fi

echo "P0-08 logs verification passed"
