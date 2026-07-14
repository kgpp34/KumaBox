#!/usr/bin/env bash
set -euo pipefail

kumabox=./bin/kumabox
root_dir=/tmp/kumabox-p0/data
run_dir=/tmp/kumabox-p0/run
log_dir=/tmp/kumabox-p0/logs

while (($#)); do
  case "$1" in
    --kumabox) kumabox=$2; shift 2 ;;
    --root-dir) root_dir=$2; shift 2 ;;
    --run-dir) run_dir=$2; shift 2 ;;
    --log-dir) log_dir=$2; shift 2 ;;
    -h|--help) echo "Usage: verify-snapshot-gc.sh [--kumabox PATH] [--root-dir PATH] [--run-dir PATH] [--log-dir PATH]"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

step() { printf '\n==> %s\n' "$1"; }
orphan_storage=$root_dir/storage/vms/kb-p4-gc-orphan-$$
orphan_staging=$root_dir/snapshot/staging/capture-p4-gc-orphan-$$
cleanup() { rm -rf "$orphan_storage" "$orphan_staging"; }
trap cleanup EXIT

step "run fail-closed GC and lease specifications"
GOCACHE=${GOCACHE:-/tmp/kumabox-go-build-cache} go test ./internal/gc ./internal/snapshot -run 'TestDryRunReportsSnapshot|TestStoreRemoveRejectsActiveReadLease' -count=1 -v

step "create recognizable orphan fixtures outside fixtures/"
mkdir -p "$orphan_storage" "$orphan_staging"
echo "state: orphan VM storage=$orphan_storage"
echo "state: orphan snapshot staging=$orphan_staging"

step "inspect GC dry-run report"
report=$("$kumabox" --root-dir "$root_dir" --run-dir "$run_dir" --log-dir "$log_dir" gc --dry-run --json)
printf '%s\n' "$report" | jq .
jq -e --arg path "$orphan_storage" '.candidates[] | select(.path == $path and .type == "orphan_vm_storage")' <<<"$report" >/dev/null
jq -e --arg path "$orphan_staging" '.candidates[] | select(.path == $path and .type == "orphan_snapshot_staging")' <<<"$report" >/dev/null

echo "P4 snapshot query, delete, and GC verification passed"
