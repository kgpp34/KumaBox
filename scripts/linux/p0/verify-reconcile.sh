#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
root_dir="/tmp/kumabox-p0/data"
run_dir="/tmp/kumabox-p0/run"
log_dir="/tmp/kumabox-p0/logs"
cloud_hypervisor_path="cloud-hypervisor"
name="p0-reconcile"
root_disk=""
kernel=""
initrd=""
firmware=""

usage() {
  cat <<'USAGE'
Usage:
  scripts/linux/p0/verify-reconcile.sh --root-disk PATH --firmware PATH [options]
  scripts/linux/p0/verify-reconcile.sh --root-disk PATH --kernel PATH --initrd PATH [options]

Options:
  --kumabox PATH             kumabox binary path, defaults to ./bin/kumabox
  --cloud-hypervisor PATH    cloud-hypervisor binary path, defaults to cloud-hypervisor
  --root-dir PATH            store directory, defaults to /tmp/kumabox-p0/data
  --run-dir PATH             runtime directory, defaults to /tmp/kumabox-p0/run
  --log-dir PATH             log directory, defaults to /tmp/kumabox-p0/logs
  --name NAME                VM name, defaults to p0-reconcile
  --firmware PATH            UEFI firmware path for cloud-image boot

Runs the P0-06 reconciliation path inside a Linux VM with KVM:
run VM -> kill recorded VMM pid -> ps/inspect must report non-RUNNING observedState.
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

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "verify-reconcile must run inside the Linux VM" >&2
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

before_json="$("$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  inspect "$name" --json)"

printf '%s\n' "$before_json"

pid="$(printf '%s' "$before_json" | sed -n 's/.*"pid": \([0-9][0-9]*\).*/\1/p' | head -n 1)"
log_path="$(printf '%s' "$before_json" | sed -n 's/.*"logDir": "\([^"]*\)".*/\1/p' | head -n 1)"

if [[ -z "$pid" ]]; then
  echo "could not parse pid from inspect output" >&2
  exit 1
fi

if [[ -z "$log_path" ]]; then
  echo "could not parse logDir from inspect output" >&2
  exit 1
fi

if ! kill -0 "$pid" 2>/dev/null; then
  echo "recorded pid is not running before reconciliation test: $pid" >&2
  exit 1
fi

kill -9 "$pid"

for _ in $(seq 1 50); do
  if ! kill -0 "$pid" 2>/dev/null; then
    break
  fi
  sleep 0.1
done

ps_json="$("$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  ps --json)"

inspect_json="$("$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  inspect "$name" --json)"

printf '%s\n' "$ps_json"
printf '%s\n' "$inspect_json"

if printf '%s' "$inspect_json" | grep -q '"observedState": "RUNNING"'; then
  echo "inspect still reports observedState RUNNING after VMM was killed" >&2
  exit 1
fi

if ! printf '%s' "$inspect_json" | grep -Eq '"observedState": "(STOPPED|FAILED|UNKNOWN)"'; then
  echo "inspect did not report STOPPED, FAILED, or UNKNOWN observedState" >&2
  exit 1
fi

if ! printf '%s' "$inspect_json" | grep -q '"observedReason":'; then
  echo "inspect output is missing observedReason" >&2
  exit 1
fi

if ! printf '%s' "$ps_json" | grep -Eq '"observedState": "(STOPPED|FAILED|UNKNOWN)"'; then
  echo "ps did not report reconciled observedState" >&2
  exit 1
fi

events_log="$log_path/events.log"
if [[ ! -f "$events_log" ]]; then
  echo "events.log was not created: $events_log" >&2
  exit 1
fi

if ! grep -q '"type":"backend.exit.detected"' "$events_log"; then
  echo "events.log does not contain backend.exit.detected" >&2
  exit 1
fi

echo "P0-06 reconciliation verification passed"
