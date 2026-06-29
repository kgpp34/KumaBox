#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
root_dir="/tmp/kumabox-p0/data"
run_dir="/tmp/kumabox-p0/run"
log_dir="/tmp/kumabox-p0/logs"
cloud_hypervisor_path="cloud-hypervisor"
name="p0-delete"
root_disk=""
kernel=""
initrd=""
firmware=""

usage() {
  cat <<'USAGE'
Usage:
  scripts/linux/verify-delete.sh --root-disk PATH --firmware PATH [options]
  scripts/linux/verify-delete.sh --root-disk PATH --kernel PATH --initrd PATH [options]

Options:
  --kumabox PATH             kumabox binary path, defaults to ./bin/kumabox
  --cloud-hypervisor PATH    cloud-hypervisor binary path, defaults to cloud-hypervisor
  --root-dir PATH            store directory, defaults to /tmp/kumabox-p0/data
  --run-dir PATH             runtime directory, defaults to /tmp/kumabox-p0/run
  --log-dir PATH             log directory, defaults to /tmp/kumabox-p0/logs
  --name NAME                VM name, defaults to p0-delete
  --firmware PATH            UEFI firmware path for cloud-image boot

Runs the P0-09 delete path:
create VM -> delete VM -> root disk must remain -> VM must disappear from ps.
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
  --cloud-hypervisor "$cloud_hypervisor_path"

create_args=(
  "$kumabox_path"
  --root-dir "$root_dir"
  --run-dir "$run_dir"
  --log-dir "$log_dir"
  --cloud-hypervisor-bin "$cloud_hypervisor_path"
  create
  --name "$name"
  --root-disk "$root_disk"
)
if [[ -n "$firmware" ]]; then
  create_args+=(--firmware "$firmware")
else
  create_args+=(--kernel "$kernel" --initrd "$initrd")
fi

created_json="$("${create_args[@]}")"
printf '%s\n' "$created_json"

run_path="$(printf '%s' "$created_json" | sed -n 's/.*"runDir": "\([^"]*\)".*/\1/p' | head -n 1)"
log_path="$(printf '%s' "$created_json" | sed -n 's/.*"logDir": "\([^"]*\)".*/\1/p' | head -n 1)"

delete_json="$("$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  delete "$name")"

printf '%s\n' "$delete_json"

if [[ ! -f "$root_disk" ]]; then
  echo "root disk was deleted unexpectedly: $root_disk" >&2
  exit 1
fi

if [[ -n "$run_path" && -e "$run_path" ]]; then
  echo "run directory still exists after delete: $run_path" >&2
  exit 1
fi

if [[ -n "$log_path" && -e "$log_path" ]]; then
  echo "log directory still exists after delete: $log_path" >&2
  exit 1
fi

ps_json="$("$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  ps --json)"

printf '%s\n' "$ps_json"

if printf '%s' "$ps_json" | grep -q "\"name\": \"$name\""; then
  echo "deleted VM still appears in ps output" >&2
  exit 1
fi

if "$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  inspect "$name" --json >/tmp/kumabox-delete-inspect.out 2>&1; then
  echo "inspect unexpectedly succeeded after delete" >&2
  cat /tmp/kumabox-delete-inspect.out >&2
  exit 1
fi

echo "P0-09 delete verification passed"
