#!/usr/bin/env bash
set -euo pipefail

kumabox=./bin/kumabox
cloud_hypervisor=cloud-hypervisor
qemu_img=qemu-img
root_dir=/tmp/kumabox-p0/data
run_dir=/tmp/kumabox-p0/run
log_dir=/tmp/kumabox-p0/logs
image=p3-agent-image
name=p5-gc-source
snapshot_name=p5-gc-native
storage=64M
use_sudo=false
success=false
manifest_path=
manifest_backup=
stale_orphan="$root_dir/snapshot/staging/p5-gc-orphan-old"
fresh_orphan="$root_dir/snapshot/staging/p5-gc-orphan-fresh"
restore_staging=

usage() {
  cat <<'EOF'
Usage: verify-native-snapshot-gc.sh [options]

  --kumabox PATH
  --cloud-hypervisor PATH
  --qemu-img PATH
  --root-dir PATH
  --run-dir PATH
  --log-dir PATH
  --image REF
  --name NAME
  --snapshot NAME
  --storage SIZE
  --sudo
EOF
}

while (($#)); do
  case "$1" in
    --kumabox) kumabox=$2; shift 2 ;;
    --cloud-hypervisor) cloud_hypervisor=$2; shift 2 ;;
    --qemu-img) qemu_img=$2; shift 2 ;;
    --root-dir) root_dir=$2; stale_orphan="$root_dir/snapshot/staging/p5-gc-orphan-old"; fresh_orphan="$root_dir/snapshot/staging/p5-gc-orphan-fresh"; shift 2 ;;
    --run-dir) run_dir=$2; shift 2 ;;
    --log-dir) log_dir=$2; shift 2 ;;
    --image) image=$2; shift 2 ;;
    --name) name=$2; shift 2 ;;
    --snapshot) snapshot_name=$2; shift 2 ;;
    --storage) storage=$2; shift 2 ;;
    --sudo) use_sudo=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ $(uname -s) == Linux ]] || { echo "native snapshot GC verification must run on Linux" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 1; }
if [[ $use_sudo == true ]]; then kb_prefix=(sudo); else kb_prefix=(); fi

step() { printf '\n==> %s\n' "$1"; }
kb() {
  "${kb_prefix[@]}" "$kumabox" \
    --root-dir "$root_dir" --run-dir "$run_dir" --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor" --qemu-img-bin "$qemu_img" "$@"
}
host() { "${kb_prefix[@]}" "$@"; }
digest_path() {
  local digest=$1 algorithm value
  algorithm=${digest%%:*}
  value=${digest#*:}
  [[ $algorithm == sha256 && $value =~ ^[0-9a-f]{64}$ ]] || {
    echo "invalid SHA-256 digest in snapshot manifest: $digest" >&2
    return 1
  }
  printf '%s/%s' "$algorithm" "$value"
}
cleanup() {
  if [[ -n $manifest_backup && -e $manifest_backup ]]; then
    host mv -f "$manifest_backup" "$manifest_path" || true
  fi
  kb delete "$name" --force >/dev/null 2>&1 || true
  kb snapshot rm "$snapshot_name" >/dev/null 2>&1 || true
  host rm -rf "$stale_orphan" "$fresh_orphan" || true
  [[ -z $restore_staging ]] || host rm -rf "$restore_staging" || true
}
on_exit() {
  [[ $success == true ]] && { cleanup; return; }
  step "preserving failed native GC state"
  kb inspect "$name" --json 2>/dev/null || true
  kb snapshot inspect "$snapshot_name" --json 2>/dev/null || true
  printf 'state: root_dir=%s run_dir=%s log_dir=%s\n' "$root_dir" "$run_dir" "$log_dir"
}
trap on_exit EXIT

step "clean previous verification state"
cleanup

step "environment checks"
scripts/linux/env-check.sh --kumabox "$kumabox" --cloud-hypervisor "$cloud_hypervisor" --qemu-img "$qemu_img" --strict

step "run VM and capture a ready native snapshot"
run_json=$(kb run "$image" --name "$name" --network none --storage "$storage")
vm_id=$(jq -r '.id' <<<"$run_json")
snapshot_json=$(kb snapshot create "$vm_id" --name "$snapshot_name" --type running --consistent crash)
snapshot_id=$(jq -r '.id' <<<"$snapshot_json")
data_dir=$(jq -r '.dataDir' <<<"$snapshot_json")
manifest_path="$data_dir/snapshot.json"
manifest_backup="$data_dir/.snapshot.json.gc-backup"
restore_staging=$(jq -r '.runDir' <<<"$run_json")/.restore-staging
printf '%s\n' "$snapshot_json" | jq .

step "create old and fresh managed staging fixtures"
host mkdir -p "$stale_orphan" "$fresh_orphan" "$restore_staging"
host touch -d '2 hours ago' "$stale_orphan" "$restore_staging"
printf 'state: stale_snapshot_staging=%s\nstate: fresh_snapshot_staging=%s\nstate: stale_restore_staging=%s\n' "$stale_orphan" "$fresh_orphan" "$restore_staging"

step "inspect explained GC candidates"
report=$(kb gc --dry-run --json)
printf '%s\n' "$report" | jq '.candidates | map(select(.component == "snapshot"))'
jq -e --arg old "$stale_orphan" --arg fresh "$fresh_orphan" --arg restore "$restore_staging" --arg ready "$data_dir" '
  any(.candidates[]; .path == $old and .type == "orphan_snapshot_staging") and
  any(.candidates[]; .path == $restore and .type == "stale_restore_staging") and
  (all(.candidates[]; .path != $fresh)) and
  (all(.candidates[]; .path != $ready))
' <<<"$report" >/dev/null

step "prove corrupt ready metadata makes GC fail closed"
host cp "$manifest_path" "$manifest_backup"
printf '{' | "${kb_prefix[@]}" tee "$manifest_path" >/dev/null
gc_error=$(mktemp)
if kb gc --dry-run --json >/dev/null 2>"$gc_error"; then
  echo "GC unexpectedly accepted a corrupt ready manifest" >&2
  exit 1
fi
cat "$gc_error"
grep -q 'read ready snapshot' "$gc_error"
host mv -f "$manifest_backup" "$manifest_path"
manifest_backup=
rm -f "$gc_error"

step "remove source VM and prove snapshot-only assets remain live"
manifest=$(host jq . "$manifest_path")
kb delete "$vm_id" --force | jq '{id,name,state}'
report=$(kb gc --dry-run --json)
kernel_digest=$(jq -r '.boot.kernelDigest // empty' <<<"$manifest")
initrd_digest=$(jq -r '.boot.initrdDigest // empty' <<<"$manifest")
layer_digests=$(jq -r '.base.layerDigests[]? // empty' <<<"$manifest")
manifest_digest=$(jq -r '.base.digest // empty' <<<"$manifest")
for digest in $kernel_digest $initrd_digest; do
  [[ -z $digest ]] && continue
  path="$root_dir/oci/boot/blobs/$(digest_path "$digest")"
  host test -f "$path"
  jq -e --arg path "$path" 'all(.candidates[]; .path != $path)' <<<"$report" >/dev/null
  printf 'pass: snapshot keeps boot asset live: %s\n' "$path"
done
if [[ -n $manifest_digest ]]; then
  path="$root_dir/oci/content/blobs/$(digest_path "$manifest_digest")"
  host test -f "$path"
  jq -e --arg path "$path" 'all(.candidates[]; .path != $path)' <<<"$report" >/dev/null
  printf 'pass: snapshot keeps OCI manifest content live: %s\n' "$path"
fi
for digest in $layer_digests; do
  digest_rel=$(digest_path "$digest")
  path="$root_dir/oci/erofs/blobs/$digest_rel.erofs"
  host test -f "$path"
  jq -e --arg path "$path" 'all(.candidates[]; .path != $path)' <<<"$report" >/dev/null
  printf 'pass: snapshot keeps EROFS layer live: %s\n' "$path"
  path="$root_dir/oci/content/blobs/$digest_rel"
  host test -f "$path"
  jq -e --arg path "$path" 'all(.candidates[]; .path != $path)' <<<"$report" >/dev/null
  printf 'pass: snapshot keeps OCI layer content live: %s\n' "$path"
done

step "remove snapshot and staging fixtures"
kb snapshot rm "$snapshot_id" | jq .
host rm -rf "$stale_orphan" "$fresh_orphan" "$restore_staging"

success=true
echo "P5 native snapshot GC verification passed"
