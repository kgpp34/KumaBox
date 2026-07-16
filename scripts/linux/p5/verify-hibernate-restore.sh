#!/usr/bin/env bash
set -euo pipefail

kumabox=./bin/kumabox
cloud_hypervisor=cloud-hypervisor
qemu_img=qemu-img
root_dir=/tmp/kumabox-p0/data
run_dir=/tmp/kumabox-p0/run
log_dir=/tmp/kumabox-p0/logs
image=p3-agent-image
name=p5-hibernate
snapshot_name=p5-hibernate-nap
storage=64M
agent_timeout=180s
use_sudo=false
success=false
start_err=
rm_err=

usage() {
  cat <<'EOF'
Usage: verify-hibernate-restore.sh [options]

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
  --agent-timeout DURATION
  --sudo
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
    --agent-timeout) agent_timeout=$2; shift 2 ;;
    --sudo) use_sudo=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ $(uname -s) == Linux ]] || { echo "hibernate verification must run on Linux" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 1; }
if [[ $use_sudo == true ]]; then kb_prefix=(sudo); else kb_prefix=(); fi

step() { printf '\n==> %s\n' "$1"; }
kb() {
  "${kb_prefix[@]}" "$kumabox" \
    --root-dir "$root_dir" --run-dir "$run_dir" --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor" --qemu-img-bin "$qemu_img" "$@"
}
cleanup() {
  kb delete "$name" --force >/dev/null 2>&1 || true
  kb snapshot rm "$snapshot_name" >/dev/null 2>&1 || true
	[[ -z $start_err ]] || rm -f "$start_err"
	[[ -z $rm_err ]] || rm -f "$rm_err"
}
on_exit() {
  [[ $success == true ]] && { cleanup; return; }
  step "preserving failed hibernate state"
  kb inspect "$name" --json 2>/dev/null || true
  kb snapshot inspect "$snapshot_name" --json 2>/dev/null || true
  printf 'state: root_dir=%s run_dir=%s log_dir=%s\n' "$root_dir" "$run_dir" "$log_dir"
}
trap on_exit EXIT

step "clean previous verification state"
cleanup

step "environment checks"
scripts/linux/env-check.sh --kumabox "$kumabox" --cloud-hypervisor "$cloud_hypervisor" --qemu-img "$qemu_img" --strict

step "run source VM and create memory, tmpfs, and disk state"
run_json=$(kb run "$image" --name "$name" --network none --storage "$storage")
vm_id=$(jq -r '.id' <<<"$run_json")
vmm_pid=$(jq -r '.pid' <<<"$run_json")
printf '%s\n' "$run_json" | jq '{id,name,state,pid,vsockSocket}'
kb agent ping "$vm_id" --timeout "$agent_timeout" | jq .
guest_pid=$(kb exec "$vm_id" -- sh -c 'nohup sh -c '\''while :; do sleep 1; done'\'' >/dev/null 2>&1 & echo $!')
kb exec "$vm_id" -- sh -c 'printf tmpfs-state > /run/kumabox-hibernate-memory; printf disk-state > /var/tmp/kumabox-hibernate-disk; sync'
printf 'state: vmm_pid=%s guest_pid=%s\n' "$vmm_pid" "$guest_pid"

step "hibernate without a resume gap"
hibernate_json=$(kb hibernate "$vm_id" --name "$snapshot_name" --consistent crash)
printf '%s\n' "$hibernate_json" | jq '{vm:{id:.vm.id,name:.vm.name,state:.vm.state,pid:.vm.pid,hibernate:.vm.hibernate},snapshot:.snapshot}'
snapshot_id=$(jq -r '.snapshot.id' <<<"$hibernate_json")
jq -e --arg snapshot "$snapshot_id" '.vm.state == "stopped" and .vm.pid == null and .vm.hibernate.snapshotId == $snapshot' <<<"$hibernate_json" >/dev/null
if "${kb_prefix[@]}" kill -0 "$vmm_pid" 2>/dev/null; then
  echo "VMM process is still alive after hibernate" >&2
  exit 1
fi

step "cold start and snapshot deletion must be rejected"
start_err=$(mktemp)
rm_err=$(mktemp)
if kb start "$vm_id" >/dev/null 2>"$start_err"; then
  echo "cold start unexpectedly succeeded" >&2
  exit 1
fi
grep -q 'VM_HIBERNATED' "$start_err"
if kb snapshot rm "$snapshot_id" >/dev/null 2>"$rm_err"; then
  echo "hibernate snapshot was deleted while VM depended on it" >&2
  exit 1
fi
grep -q 'SNAPSHOT_IN_USE' "$rm_err"

step "wake by restoring the original VM identity"
restore_json=$(kb restore "$vm_id" "$snapshot_id" --restore-mode copy)
printf '%s\n' "$restore_json" | jq '{id,name,state,pid,hibernate,lastRestore}'
jq -e '.state == "running" and (.hibernate == null) and .lastRestore.mode == "copy"' <<<"$restore_json" >/dev/null
kb agent ping "$vm_id" --timeout "$agent_timeout" | jq .
kb exec "$vm_id" -- sh -c "kill -0 $guest_pid; test \"\$(cat /run/kumabox-hibernate-memory)\" = tmpfs-state; test \"\$(cat /var/tmp/kumabox-hibernate-disk)\" = disk-state"

step "remove verification resources"
kb delete "$vm_id" --force | jq '{id,name,state}'
kb snapshot rm "$snapshot_id" | jq .
rm -f "$start_err" "$rm_err"
start_err=
rm_err=

success=true
echo "P5 hibernate and restore verification passed"
