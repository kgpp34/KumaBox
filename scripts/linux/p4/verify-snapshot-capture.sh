#!/usr/bin/env bash
set -euo pipefail

kumabox=./bin/kumabox
cloud_hypervisor=cloud-hypervisor
qemu_img=qemu-img
root_dir=/tmp/kumabox-p0/data
run_dir=/tmp/kumabox-p0/run
log_dir=/tmp/kumabox-p0/logs
image=ubuntu
name=p4-snapshot
storage=64M
keep=false

usage() {
  cat <<'EOF'
Usage: verify-snapshot-capture.sh [options]

  --kumabox PATH
  --cloud-hypervisor PATH
  --qemu-img PATH
  --root-dir PATH
  --run-dir PATH
  --log-dir PATH
  --image REF
  --name NAME
  --storage SIZE
  --sudo                 accepted for compatibility; network=none needs no sudo
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
    --storage) storage=$2; shift 2 ;;
    --sudo) shift ;;
    --keep) keep=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

step() { printf '\n==> %s\n' "$1"; }
kb() {
  "$kumabox" --root-dir "$root_dir" --run-dir "$run_dir" --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor" --qemu-img-bin "$qemu_img" "$@"
}

vm_name=${name}-vm
snapshot_name=${name}-disk
vm_id=
snapshot_id=
success=false

cleanup() {
	if [[ $success == true ]]; then
		if [[ $keep == false ]]; then
			[[ -z $snapshot_id ]] || kb snapshot rm "$snapshot_id" >/dev/null 2>&1 || true
			[[ -z $vm_id ]] || kb delete "$vm_id" --force >/dev/null 2>&1 || true
		fi
		return
	fi

	echo
	echo "==> preserving failed stopped snapshot state"
	echo "state: root_dir=$root_dir run_dir=$run_dir log_dir=$log_dir"
	if [[ -n $vm_id ]]; then
		step "failure context: VM inspect"
		inspect=$(kb inspect "$vm_id" --json 2>/dev/null || true)
		printf '%s\n' "$inspect"
		vm_log_dir=$(jq -r '.logDir // empty' <<<"$inspect" 2>/dev/null || true)
		vm_config=$(jq -r '.config // empty' <<<"$inspect" 2>/dev/null || true)
		if [[ -n $vm_config ]]; then
			step "failure context: rendered config"
			cat "$vm_config" 2>/dev/null | jq . || true
		fi
		if [[ -n $vm_log_dir ]]; then
			step "failure context: cloud-hypervisor stderr"
			tail -n 120 "$vm_log_dir/cloud-hypervisor.stderr.log" 2>/dev/null || true
		fi
	fi
}
trap cleanup EXIT

step "clean previous verification records"
if kb inspect "$vm_name" --json >/dev/null 2>&1; then
  kb delete "$vm_name" --force >/dev/null
fi
if kb snapshot inspect "$snapshot_name" --json >/dev/null 2>&1; then
  kb snapshot rm "$snapshot_name" >/dev/null
fi

step "run VM from managed image"
vm_json=$(kb run "$image" --name "$vm_name" --network none --storage "$storage")
printf '%s\n' "$vm_json" | jq .
vm_id=$(jq -r '.id' <<<"$vm_json")

step "prove running snapshot is rejected"
if kb snapshot create "$vm_id" --name "$snapshot_name" >/tmp/kumabox-p4-running-snapshot.out 2>&1; then
  echo "running snapshot unexpectedly succeeded" >&2
  exit 1
fi
cat /tmp/kumabox-p4-running-snapshot.out
grep -q 'VM_RUNNING' /tmp/kumabox-p4-running-snapshot.out || { echo "missing VM_RUNNING error" >&2; exit 1; }

step "stop VM and capture all writable disks"
kb stop "$vm_id" | jq .
snapshot_json=$(kb snapshot create "$vm_id" --name "$snapshot_name")
printf '%s\n' "$snapshot_json" | jq .
snapshot_id=$(jq -r '.id' <<<"$snapshot_json")
data_dir=$(jq -r '.dataDir' <<<"$snapshot_json")

step "inspect published snapshot and manifest"
kb snapshot inspect "$snapshot_id" --json | jq .
manifest=$data_dir/snapshot.json
jq . "$manifest"
jq -e '.schemaVersion == "kumabox.snapshot.v1" and .type == "disk" and .consistency == "stopped-disk" and (.disks | length > 0)' "$manifest" >/dev/null

step "verify every payload checksum and copy strategy"
while IFS=$'\t' read -r relative expected strategy; do
  payload=$data_dir/$relative
  actual=$(sha256sum "$payload" | awk '{print $1}')
  printf 'state: payload=%s strategy=%s sha256=%s\n' "$payload" "$strategy" "$actual"
  [[ $actual == "$expected" ]] || { echo "checksum mismatch for $relative" >&2; exit 1; }
  [[ -n $strategy && $strategy != null ]] || { echo "copy strategy missing for $relative" >&2; exit 1; }
done < <(jq -r '.disks[] | [.path, .sha256, .copyStrategy] | @tsv' "$manifest")

success=true
if [[ $keep == true ]]; then
  echo "state: kept vm=$vm_id snapshot=$snapshot_id for export/import/restore verification"
fi
echo "P4 stopped snapshot capture verification passed"
