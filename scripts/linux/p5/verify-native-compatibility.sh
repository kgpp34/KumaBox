#!/usr/bin/env bash
set -euo pipefail

kumabox=./bin/kumabox
cloud_hypervisor=cloud-hypervisor
qemu_img=qemu-img
root_dir=/tmp/kumabox-p0/data
run_dir=/tmp/kumabox-p0/run
log_dir=/tmp/kumabox-p0/logs
image=ubuntu
name=p5-compat
snapshot_name=p5-compat-native
memory=512M
storage=64M
use_sudo=false
success=false
vm_id=
snapshot_id=
backup=
state_file=

usage() {
  cat <<'EOF'
Usage: verify-native-compatibility.sh [options]

  --kumabox PATH
  --cloud-hypervisor PATH
  --qemu-img PATH
  --root-dir PATH
  --run-dir PATH
  --log-dir PATH
  --image REF
  --name NAME
  --snapshot NAME
  --memory SIZE
  --storage SIZE
  --sudo

Only the named VM and snapshot are cleaned. A failure preserves state and
prints the paths needed for manual inspection.
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
    --memory) memory=$2; shift 2 ;;
    --storage) storage=$2; shift 2 ;;
    --sudo) use_sudo=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ $(uname -s) == Linux ]] || { echo "native compatibility verification must run on Linux" >&2; exit 1; }
for command in jq cp truncate; do
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
  if [[ -n $backup ]] && "${file_prefix[@]}" test -f "$backup"; then
    "${file_prefix[@]}" mv -f "$backup" "$state_file" || true
  fi
  if [[ $success == true ]]; then
    clean_named_state
    return
  fi
  step "preserving failed compatibility verification state"
  [[ -z $vm_id ]] || kb inspect "$vm_id" --json 2>/dev/null || true
  [[ -z $snapshot_id ]] || kb snapshot inspect "$snapshot_id" --json 2>/dev/null || true
  printf 'state: root_dir=%s run_dir=%s log_dir=%s\n' "$root_dir" "$run_dir" "$log_dir"
}
trap on_exit EXIT

step "clean previous named verification state"
clean_named_state

step "environment checks"
scripts/linux/env-check.sh \
  --kumabox "$kumabox" --cloud-hypervisor "$cloud_hypervisor" --qemu-img "$qemu_img" --strict

step "run source VM with explicit machine shape"
vm_json=$(kb run "$image" --name "$name" --network none --cpus 1 --memory "$memory" --storage "$storage")
printf '%s\n' "$vm_json" | jq '{id, name, state, observedState, cpus, memoryBytes, storageConfigs}'
vm_id=$(jq -r '.id' <<<"$vm_json")

step "capture v2 native snapshot"
snapshot_json=$(kb snapshot create "$vm_id" --name "$snapshot_name" --type running --consistent crash)
printf '%s\n' "$snapshot_json" | jq .
snapshot_id=$(jq -r '.id' <<<"$snapshot_json")
data_dir=$(jq -r '.dataDir' <<<"$snapshot_json")

step "verify payload and compatibility before mutation"
verify_json=$(kb snapshot verify "$snapshot_id" --vm "$vm_id")
printf '%s\n' "$verify_json" | jq '{schemaVersion, type, consistency, backend, machine, boot, devices, network}'
jq -e '
  .schemaVersion == "kumabox.snapshot.v2" and
  .type == "native" and
  .backend.name == "cloud-hypervisor" and
  .backend.snapshotFormat == "cloud-hypervisor-native-v1" and
  (.machine.memoryBytes > 0) and
  (.native.files | all(.sha256 | length == 64))
' <<<"$verify_json" >/dev/null

step "show checksum inventory"
"${file_prefix[@]}" cat "$data_dir/checksums.txt"

step "corrupt native state and prove preflight fails closed"
state_file=$data_dir/native/state.json
backup=/tmp/kumabox-p5-compat-${snapshot_id}.state
"${file_prefix[@]}" cp --reflink=auto "$state_file" "$backup"
"${file_prefix[@]}" truncate -s 1 "$state_file"
if verify_error=$(kb snapshot verify "$snapshot_id" --vm "$vm_id" 2>&1); then
  echo "corrupt native snapshot unexpectedly passed verification" >&2
  exit 1
fi
printf '%s\n' "$verify_error"
grep -Eq 'SNAPSHOT_CORRUPT|CHECKSUM_MISMATCH' <<<"$verify_error" || {
  echo "verification did not report snapshot corruption" >&2
  exit 1
}

step "restore payload and verify again"
"${file_prefix[@]}" mv -f "$backup" "$state_file"
backup=
kb snapshot verify "$snapshot_id" --vm "$vm_id" | jq '{schemaVersion, backend, machine}'

step "prove source VM was never stopped by preflight"
kb inspect "$vm_id" --json | jq -e '.state == "running" and .observedState == "RUNNING"' >/dev/null
echo "pass: source VM remains running"

step "remove verification resources"
kb stop "$vm_id" | jq '{id, state, observedState}'
kb delete "$vm_id" | jq '{id, name, state}'
kb snapshot rm "$snapshot_id" | jq .
vm_id=
snapshot_id=

success=true
echo "P5 native compatibility verification passed"
