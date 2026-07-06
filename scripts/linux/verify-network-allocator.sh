#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
root_dir="/tmp/kumabox-p0/data"
run_dir="/tmp/kumabox-p0/run"
log_dir="/tmp/kumabox-p0/logs"

usage() {
  cat <<'USAGE'
Usage: scripts/linux/verify-network-allocator.sh [options]

Verifies P2-02 Tap/MAC/IP allocator behavior without creating host tap devices,
bridges, NAT rules, or VMs.

Options:
  --kumabox PATH     kumabox binary path
  --root-dir PATH    persistent state directory
  --run-dir PATH     runtime directory
  --log-dir PATH     log directory
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --kumabox)
      kumabox_path="${2:-}"
      shift 2
      ;;
    --root-dir)
      root_dir="${2:-}"
      shift 2
      ;;
    --run-dir)
      run_dir="${2:-}"
      shift 2
      ;;
    --log-dir)
      log_dir="${2:-}"
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

if [[ -z "$kumabox_path" || -z "$root_dir" || -z "$run_dir" || -z "$log_dir" ]]; then
  usage >&2
  exit 2
fi

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "verify-network-allocator must run on Linux" >&2
  exit 1
fi

rm -rf "$root_dir/network"
mkdir -p "$root_dir" "$run_dir" "$log_dir"

go test ./internal/network -run 'TestAllocator|TestReleaseIP' -count=1

network_ls="$("$kumabox_path" \
  --root-dir "$root_dir" \
  network ls --json)"
if [[ "$network_ls" != "[]" ]]; then
  echo "network ls expected empty JSON list, got: $network_ls" >&2
  exit 1
fi

echo "P2-02 network allocator verification passed"
