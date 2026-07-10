#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
root_dir="/tmp/kumabox-p0/data"
run_dir="/tmp/kumabox-p0/run"
log_dir="/tmp/kumabox-p0/logs"
orphan_id="kb_orphan_gc"

usage() {
  cat <<'USAGE'
Usage: scripts/linux/p0/verify-gc.sh [options]

Options:
  --kumabox PATH   kumabox binary path, defaults to ./bin/kumabox
  --root-dir PATH  store directory, defaults to /tmp/kumabox-p0/data
  --run-dir PATH   runtime directory, defaults to /tmp/kumabox-p0/run
  --log-dir PATH   log directory, defaults to /tmp/kumabox-p0/logs

Runs the P0-10 GC dry-run path:
create orphan run/log dirs -> gc --dry-run --json reports candidates -> dirs remain.
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

if [[ ! -x "$kumabox_path" ]]; then
  echo "kumabox is not executable: $kumabox_path" >&2
  exit 1
fi

orphan_run="$run_dir/vms/$orphan_id"
orphan_log="$log_dir/vms/$orphan_id"
mkdir -p "$orphan_run" "$orphan_log"
printf 'orphan\n' >"$orphan_run/ch.pid"

gc_json="$("$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  gc --dry-run --json)"

printf '%s\n' "$gc_json"

if ! printf '%s' "$gc_json" | grep -q "\"path\": \"$orphan_run\""; then
  echo "GC dry-run did not report orphan run dir" >&2
  exit 1
fi

if ! printf '%s' "$gc_json" | grep -q "\"path\": \"$orphan_log\""; then
  echo "GC dry-run did not report orphan log dir" >&2
  exit 1
fi

if [[ ! -d "$orphan_run" || ! -d "$orphan_log" ]]; then
  echo "GC dry-run deleted a candidate unexpectedly" >&2
  exit 1
fi

echo "P0-10 GC dry-run verification passed"
