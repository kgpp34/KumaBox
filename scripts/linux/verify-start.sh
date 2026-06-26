#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
root_dir="/tmp/kumabox-p0/data"
run_dir="/tmp/kumabox-p0/run"
log_dir="/tmp/kumabox-p0/logs"
cloud_hypervisor_path="cloud-hypervisor"
name="p0-start"
root_disk=""
kernel=""
initrd=""

usage() {
  cat <<'USAGE'
Usage: scripts/linux/verify-start.sh --root-disk PATH --kernel PATH --initrd PATH [options]

Options:
  --kumabox PATH             kumabox binary path, defaults to ./bin/kumabox
  --cloud-hypervisor PATH    cloud-hypervisor binary path, defaults to cloud-hypervisor
  --root-dir PATH            store directory, defaults to /tmp/kumabox-p0/data
  --run-dir PATH             runtime directory, defaults to /tmp/kumabox-p0/run
  --log-dir PATH             log directory, defaults to /tmp/kumabox-p0/logs
  --name NAME                VM name, defaults to p0-start

Runs the P0-05 create -> start -> inspect path inside a Linux VM with KVM.
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

if [[ -z "$root_disk" || -z "$kernel" || -z "$initrd" ]]; then
  usage >&2
  exit 2
fi

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "verify-start must run inside the Linux VM" >&2
  exit 1
fi

for path in "$kumabox_path" "$root_disk" "$kernel" "$initrd"; do
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

"$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  create \
  --name "$name" \
  --root-disk "$root_disk" \
  --kernel "$kernel" \
  --initrd "$initrd" >/dev/null

"$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  start "$name" >/dev/null

record_json="$("$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  inspect "$name" --json)"

printf '%s\n' "$record_json"

if ! printf '%s' "$record_json" | grep -q '"state": "running"'; then
  echo "VM did not enter running state" >&2
  exit 1
fi

if ! printf '%s' "$record_json" | grep -q '"pid":'; then
  echo "running VM record is missing pid" >&2
  exit 1
fi

if ! printf '%s' "$record_json" | grep -q '"apiSocket":'; then
  echo "running VM record is missing apiSocket" >&2
  exit 1
fi

pid="$(printf '%s' "$record_json" | sed -n 's/.*"pid": \([0-9][0-9]*\).*/\1/p' | head -n 1)"
api_socket="$(printf '%s' "$record_json" | sed -n 's/.*"apiSocket": "\([^"]*\)".*/\1/p' | head -n 1)"

if [[ -z "$pid" ]]; then
  echo "could not parse pid from inspect output" >&2
  exit 1
fi

if [[ -z "$api_socket" ]]; then
  echo "could not parse apiSocket from inspect output" >&2
  exit 1
fi

if ! kill -0 "$pid" 2>/dev/null; then
  echo "recorded pid is not running: $pid" >&2
  exit 1
fi

if [[ ! -S "$api_socket" ]]; then
  echo "apiSocket is not a Unix socket: $api_socket" >&2
  exit 1
fi

process_args="$(ps -p "$pid" -o args= 2>/dev/null || true)"
if [[ "$process_args" != *"$cloud_hypervisor_path"* && "$process_args" != *"cloud-hypervisor"* ]]; then
  echo "recorded pid does not look like cloud-hypervisor: $process_args" >&2
  exit 1
fi
