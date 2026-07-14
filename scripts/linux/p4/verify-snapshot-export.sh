#!/usr/bin/env bash
set -euo pipefail

kumabox=./bin/kumabox
root_dir=/tmp/kumabox-p0/data
snapshot=p4-snapshot-disk
output=/tmp/kumabox-p0/p4-snap.kbsnap
compression=none

while (($#)); do
  case "$1" in
    --kumabox) kumabox=$2; shift 2 ;;
    --root-dir) root_dir=$2; shift 2 ;;
    --snapshot) snapshot=$2; shift 2 ;;
    --output) output=$2; shift 2 ;;
    --compression) compression=$2; shift 2 ;;
    -h|--help) echo "Usage: verify-snapshot-export.sh [--kumabox PATH] [--root-dir PATH] [--snapshot REF] [--output PATH] [--compression none|gzip|zstd]"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

step() { printf '\n==> %s\n' "$1"; }

step "inspect source snapshot"
source_json=$("$kumabox" --root-dir "$root_dir" snapshot inspect "$snapshot" --json)
printf '%s\n' "$source_json" | jq .
data_dir=$(jq -r '.dataDir' <<<"$source_json")
manifest_sha_before=$(sha256sum "$data_dir/snapshot.json" | awk '{print $1}')

step "export sparse-aware streaming package"
rm -f "$output"
"$kumabox" --root-dir "$root_dir" snapshot export "$snapshot" --output "$output" --compression "$compression" | jq .
ls -lh "$output"

step "verify archive entry order and source immutability"
case "$compression" in
  none) entries=$(tar -tf "$output") ;;
  gzip) entries=$(tar -tzf "$output") ;;
  zstd) entries=$(zstd -dc "$output" | tar -tf -) ;;
esac
printf '%s\n' "$entries"
[[ $(head -n1 <<<"$entries") == manifest.json ]] || { echo "manifest.json is not the first entry" >&2; exit 1; }
grep -q '^checksums.txt$' <<<"$entries" || { echo "checksums.txt missing" >&2; exit 1; }
[[ $(sha256sum "$data_dir/snapshot.json" | awk '{print $1}') == "$manifest_sha_before" ]] || { echo "export modified source manifest" >&2; exit 1; }
find "$(dirname "$output")" -maxdepth 1 -name '.kumabox-export-*.partial' -print -quit | grep -q . && { echo "partial export file remains" >&2; exit 1; }

echo "P4 streaming snapshot export verification passed"
