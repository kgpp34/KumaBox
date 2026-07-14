#!/usr/bin/env bash
set -euo pipefail

kumabox=./bin/kumabox
root_dir=/tmp/kumabox-p0/data

usage() {
  cat <<'EOF'
Usage: verify-snapshot-store.sh [--kumabox PATH] [--root-dir PATH]

Verifies pending/ready transactions, name conflicts, snapshot leases, and the
public snapshot query command without starting a VM.
EOF
}

while (($#)); do
  case "$1" in
    --kumabox) kumabox=$2; shift 2 ;;
    --root-dir) root_dir=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

step() { printf '\n==> %s\n' "$1"; }

step "run snapshot store transaction specifications"
GOCACHE=${GOCACHE:-/tmp/kumabox-go-build-cache} go test ./internal/snapshot \
  -run 'TestStore|TestBuild' -count=1 -v

step "show the durable snapshot store layout"
mkdir -p "$root_dir/snapshot"
find "$root_dir/snapshot" -maxdepth 2 -mindepth 1 -print 2>/dev/null | sort || true

step "list published snapshots through the CLI"
output=$("$kumabox" --root-dir "$root_dir" snapshot ls --json)
printf '%s\n' "$output" | jq .
jq -e 'type == "array"' <<<"$output" >/dev/null

echo "P4 snapshot store and lease verification passed"
