#!/usr/bin/env bash
set -euo pipefail

kumabox=./bin/kumabox
qemu_img=qemu-img
root_dir=/tmp/kumabox-p0/data
input=/tmp/kumabox-p0/p4-snap.kbsnap
name=p4-imported
keep=false

while (($#)); do
  case "$1" in
    --kumabox) kumabox=$2; shift 2 ;;
    --qemu-img) qemu_img=$2; shift 2 ;;
    --root-dir) root_dir=$2; shift 2 ;;
    --input) input=$2; shift 2 ;;
    --name) name=$2; shift 2 ;;
    --keep) keep=true; shift ;;
    -h|--help) echo "Usage: verify-snapshot-import.sh [--kumabox PATH] [--qemu-img PATH] [--root-dir PATH] [--input FILE] [--name NAME] [--keep]"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

step() { printf '\n==> %s\n' "$1"; }
kb() { "$kumabox" --root-dir "$root_dir" --qemu-img-bin "$qemu_img" "$@"; }
imported_id=
cleanup() { [[ $keep == true || -z $imported_id ]] || kb snapshot rm "$imported_id" >/dev/null 2>&1 || true; }
trap cleanup EXIT

step "clean previous imported snapshot"
if kb snapshot inspect "$name" --json >/dev/null 2>&1; then kb snapshot rm "$name" >/dev/null; fi

step "run archive attack and checksum specifications"
GOCACHE=${GOCACHE:-/tmp/kumabox-go-build-cache} go test ./internal/snapshot -run 'TestStoreImport' -count=1 -v

step "import package through restricted staging"
json=$(kb snapshot import "$input" --name "$name")
printf '%s\n' "$json" | jq .
imported_id=$(jq -r '.id' <<<"$json")
[[ $(jq -r '.state' <<<"$json") == ready ]] || { echo "imported snapshot is not ready" >&2; exit 1; }

step "inspect rewritten identity and validated manifest"
inspect=$(kb snapshot inspect "$imported_id" --json)
printf '%s\n' "$inspect" | jq .
data_dir=$(jq -r '.dataDir' <<<"$inspect")
jq . "$data_dir/snapshot.json"
[[ $(jq -r '.id' "$data_dir/snapshot.json") == "$imported_id" ]] || { echo "manifest ID was not regenerated" >&2; exit 1; }
[[ $(jq -r '.name' "$data_dir/snapshot.json") == "$name" ]] || { echo "manifest name was not overridden" >&2; exit 1; }

if [[ $keep == true ]]; then
  echo "state: kept imported_snapshot=$imported_id for restore verification"
fi
echo "P4 secure snapshot import verification passed"
