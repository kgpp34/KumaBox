#!/usr/bin/env bash
set -euo pipefail

kumabox=./bin/kumabox
cloud_hypervisor=cloud-hypervisor
qemu_img=qemu-img
root_dir=/tmp/kumabox-p0/data
run_dir=/tmp/kumabox-p0/run
log_dir=/tmp/kumabox-p0/logs
image=ubuntu
name=p5-native
snapshot_name=p5-native-running
storage=64M
use_sudo=false
success=false
vm_id=
snapshot_id=

usage() {
  cat <<'EOF'
Usage: verify-native-capture.sh [options]

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

The script removes only the named verification VM and snapshot. Failed state is
preserved for inspection; fixtures, managed images, and unrelated VMs remain.
EOF
}

while (($#)); do
  case "$1" in
    --kumabox) kumabox=$2; shift 2 ;;
    --cloud-hypervisor) cloud_hypervisor=$2; shift 2 ;;
    --qemu-img) qemu_img=$2; shift 2 ;;
    --root-dir) root_dir=$2; shift 2 ;;
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

[[ $(uname -s) == Linux ]] || { echo "native snapshot verification must run on Linux" >&2; exit 1; }
for command in jq sha256sum; do
  command -v "$command" >/dev/null 2>&1 || { echo "$command is required" >&2; exit 1; }
done

if [[ $use_sudo == true ]]; then
  command -v sudo >/dev/null 2>&1 || { echo "--sudo requested but sudo is missing" >&2; exit 1; }
  kb_prefix=(sudo)
  file_prefix=(sudo)
else
  kb_prefix=()
  file_prefix=()
fi

step() { printf '\n==> %s\n' "$1"; }
kb() {
  "${kb_prefix[@]}" "$kumabox" \
    --root-dir "$root_dir" --run-dir "$run_dir" --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor" --qemu-img-bin "$qemu_img" "$@"
}

clean_named_state() {
  kb delete "$name" --force >/dev/null 2>&1 || true
  kb snapshot rm "$snapshot_name" >/dev/null 2>&1 || true
}

on_exit() {
  if [[ $success == true ]]; then
    clean_named_state
    return
  fi
  step "preserving failed native capture state"
  [[ -z $vm_id ]] || kb inspect "$vm_id" --json 2>/dev/null || true
  [[ -z $snapshot_id ]] || kb snapshot inspect "$snapshot_id" --json 2>/dev/null || true
  printf 'state: root_dir=%s run_dir=%s log_dir=%s\n' "$root_dir" "$run_dir" "$log_dir"
}
trap on_exit EXIT

step "clean previous named verification state"
clean_named_state

step "environment checks"
scripts/linux/env-check.sh \
  --kumabox "$kumabox" \
  --cloud-hypervisor "$cloud_hypervisor" \
  --qemu-img "$qemu_img" \
  --strict

step "inspect managed source image"
kb image inspect "$image" --json | jq '{id, name, source, rootDisk, boot}'

step "run source VM"
vm_json=$(kb run "$image" --name "$name" --network none --storage "$storage")
printf '%s\n' "$vm_json" | jq .
vm_id=$(jq -r '.id' <<<"$vm_json")

step "capture native running snapshot"
snapshot_json=$(kb snapshot create "$vm_id" --name "$snapshot_name" --type running --consistent crash)
printf '%s\n' "$snapshot_json" | jq .
snapshot_id=$(jq -r '.id' <<<"$snapshot_json")
data_dir=$(jq -r '.dataDir' <<<"$snapshot_json")

step "prove source VM resumed"
inspect_json=$(kb inspect "$vm_id" --json)
printf '%s\n' "$inspect_json" | jq '{id, name, state, observedState, pid, apiSocket}'
jq -e '.state == "running" and .observedState == "RUNNING" and (.pid > 0)' <<<"$inspect_json" >/dev/null

step "show native backend payload"
"${file_prefix[@]}" find "$data_dir/native" -maxdepth 1 -type f -printf '%f\t%s bytes\n' | sort
"${file_prefix[@]}" test -f "$data_dir/native/config.json"
"${file_prefix[@]}" test -f "$data_dir/native/state.json"
"${file_prefix[@]}" find "$data_dir/native" -maxdepth 1 -type f -name 'memory-range*' -print -quit | grep -q .

step "show published manifest"
manifest_json=$("${file_prefix[@]}" cat "$data_dir/snapshot.json")
printf '%s\n' "$manifest_json" | jq .
jq -e '
  .schemaVersion == "kumabox.snapshot.v2" and
  .type == "native" and
  .consistency == "crash" and
  (.native.files | length >= 3) and
  (.disks | length > 0)
' <<<"$manifest_json" >/dev/null

step "verify writable disk copies"
while IFS=$'\t' read -r relative expected strategy; do
  payload=$data_dir/$relative
  actual=$("${file_prefix[@]}" sha256sum "$payload" | awk '{print $1}')
  printf 'state: payload=%s strategy=%s sha256=%s\n' "$payload" "$strategy" "$actual"
  [[ $actual == "$expected" ]] || { echo "checksum mismatch for $relative" >&2; exit 1; }
  [[ -n $strategy && $strategy != null ]] || { echo "copy strategy missing for $relative" >&2; exit 1; }
done < <(jq -r '.disks[] | [.path, .sha256, .copyStrategy] | @tsv' <<<"$manifest_json")

step "stop and remove verification resources"
kb stop "$vm_id" | jq '{id, state, observedState}'
kb delete "$vm_id" | jq '{id, name, state}'
kb snapshot rm "$snapshot_id" | jq .
vm_id=
snapshot_id=

success=true
echo "P5 native running snapshot verification passed"
