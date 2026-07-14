#!/usr/bin/env bash
set -euo pipefail

kumabox=./bin/kumabox
qemu_img=qemu-img
root_dir=/tmp/kumabox-p0/data
run_dir=/tmp/kumabox-p0/run
log_dir=/tmp/kumabox-p0/logs
image=ubuntu
name_prefix=p4-overlay

usage() {
  cat <<'EOF'
Usage: verify-cloudimg-overlay.sh [options]

  --kumabox PATH   KumaBox binary (default: ./bin/kumabox)
  --qemu-img PATH  qemu-img binary (default: qemu-img)
  --root-dir PATH  durable data root
  --run-dir PATH   runtime root
  --log-dir PATH   log root
  --image REF      managed qcow2 cloud image
  --name PREFIX    verification VM name prefix
EOF
}

while (($#)); do
  case "$1" in
    --kumabox) kumabox=$2; shift 2 ;;
    --qemu-img) qemu_img=$2; shift 2 ;;
    --root-dir) root_dir=$2; shift 2 ;;
    --run-dir) run_dir=$2; shift 2 ;;
    --log-dir) log_dir=$2; shift 2 ;;
    --image) image=$2; shift 2 ;;
    --name) name_prefix=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

step() { printf '\n==> %s\n' "$1"; }
kb() {
  "$kumabox" --root-dir "$root_dir" --run-dir "$run_dir" --log-dir "$log_dir" --qemu-img-bin "$qemu_img" "$@"
}

vm_a=${name_prefix}-a
vm_b=${name_prefix}-b
created_a=
created_b=
success=false

cleanup() {
  if [[ $success == true ]]; then
    [[ -z $created_a ]] || kb delete "$created_a" >/dev/null 2>&1 || true
    [[ -z $created_b ]] || kb delete "$created_b" >/dev/null 2>&1 || true
    return
  fi
  echo
  echo "==> preserving failed cloud image overlay state"
  echo "state: root_dir=$root_dir run_dir=$run_dir log_dir=$log_dir"
}
trap cleanup EXIT

command -v "$qemu_img" >/dev/null 2>&1 || { echo "qemu-img not found: $qemu_img" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 1; }

step "clean previous verification VM records"
for name in "$vm_a" "$vm_b"; do
  if kb inspect "$name" --json >/dev/null 2>&1; then
    kb delete "$name" --force >/dev/null
  fi
done

step "inspect managed cloud image"
image_json=$(kb image inspect "$image" --json)
printf '%s\n' "$image_json" | jq .
base=$(jq -r '.rootDisk.path' <<<"$image_json")
base_format=$(jq -r '.rootDisk.format' <<<"$image_json")
base_digest=$(jq -r '.rootDisk.sha256' <<<"$image_json")
[[ $base_format == qcow2 ]] || { echo "image base format is $base_format, want qcow2" >&2; exit 1; }
[[ -n $base_digest && $base_digest != null ]] || { echo "image has no root disk sha256" >&2; exit 1; }
base_sha_before=$(sha256sum "$base" | awk '{print $1}')
base_mtime_before=$(stat -c %Y "$base")
[[ $base_sha_before == "${base_digest#sha256:}" ]] || { echo "image record digest does not match base" >&2; exit 1; }
echo "state: base=$base sha256=$base_sha_before mtime=$base_mtime_before"

step "create two VMs from the same image"
json_a=$(kb create "$image" --name "$vm_a" --network none)
json_b=$(kb create "$image" --name "$vm_b" --network none)
printf '%s\n' "$json_a" | jq .
printf '%s\n' "$json_b" | jq .
created_a=$(jq -r '.id' <<<"$json_a")
created_b=$(jq -r '.id' <<<"$json_b")
overlay_a=$(jq -r '.rootDisk' <<<"$json_a")
overlay_b=$(jq -r '.rootDisk' <<<"$json_b")
[[ $overlay_a != "$overlay_b" ]] || { echo "VM overlays unexpectedly share one path" >&2; exit 1; }
[[ $overlay_a != "$base" && $overlay_b != "$base" ]] || { echo "VM renders the shared base as writable root" >&2; exit 1; }
echo "state: vm_a=$created_a overlay_a=$overlay_a"
echo "state: vm_b=$created_b overlay_b=$overlay_b"

step "inspect both qcow2 backing chains"
chain_a=$($qemu_img info --backing-chain --output=json "$overlay_a")
chain_b=$($qemu_img info --backing-chain --output=json "$overlay_b")
printf '%s\n' "$chain_a" | jq .
printf '%s\n' "$chain_b" | jq .
[[ $(jq -r '.[0].format' <<<"$chain_a") == qcow2 ]] || { echo "VM A root is not qcow2" >&2; exit 1; }
[[ $(jq -r '.[0]["backing-filename"]' <<<"$chain_a") == "$base" ]] || { echo "VM A backing path mismatch" >&2; exit 1; }
[[ $(jq -r '.[0]["backing-filename"]' <<<"$chain_b") == "$base" ]] || { echo "VM B backing path mismatch" >&2; exit 1; }

step "verify renderer uses overlays"
config_a=$(jq -r '.config' <<<"$json_a")
config_b=$(jq -r '.config' <<<"$json_b")
jq '.disks' "$config_a"
jq '.disks' "$config_b"
[[ $(jq -r '.disks[0].path' "$config_a") == "$overlay_a" ]] || { echo "VM A renderer does not use overlay" >&2; exit 1; }
[[ $(jq -r '.disks[0].path' "$config_b") == "$overlay_b" ]] || { echo "VM B renderer does not use overlay" >&2; exit 1; }

step "verify image removal is fail-closed"
if kb image rm "$image" >/tmp/kumabox-p4-image-rm.out 2>&1; then
  echo "image rm unexpectedly removed a referenced base" >&2
  exit 1
fi
cat /tmp/kumabox-p4-image-rm.out
grep -q 'IMAGE_IN_USE' /tmp/kumabox-p4-image-rm.out || { echo "image rm did not report IMAGE_IN_USE" >&2; exit 1; }

step "delete VM A and verify VM B plus base remain"
kb delete "$created_a" | jq .
created_a=
[[ -f $overlay_b ]] || { echo "deleting VM A removed VM B overlay" >&2; exit 1; }
[[ -f $base ]] || { echo "deleting VM A removed image base" >&2; exit 1; }
base_sha_after=$(sha256sum "$base" | awk '{print $1}')
base_mtime_after=$(stat -c %Y "$base")
[[ $base_sha_after == "$base_sha_before" ]] || { echo "image base content changed" >&2; exit 1; }
[[ $base_mtime_after == "$base_mtime_before" ]] || { echo "image base mtime changed" >&2; exit 1; }
echo "pass: VM overlays are isolated and the shared base stayed immutable"

success=true
echo "P4 cloud image overlay verification passed"
