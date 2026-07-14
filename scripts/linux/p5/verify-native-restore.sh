#!/usr/bin/env bash
set -euo pipefail

kumabox=./bin/kumabox
cloud_hypervisor=cloud-hypervisor
qemu_img=qemu-img
root_dir=/tmp/kumabox-p0/data
run_dir=/tmp/kumabox-p0/run
log_dir=/tmp/kumabox-p0/logs
image=p3-agent-image
name=p5-restore
snapshot_name=p5-restore-native
storage=64M
agent_timeout=180s
use_sudo=false
success=false
vm_id=
snapshot_id=
tail_pid=

usage() {
  cat <<'EOF'
Usage: verify-native-restore.sh [options]

  --kumabox PATH
  --cloud-hypervisor PATH
  --qemu-img PATH
  --root-dir PATH
  --run-dir PATH
  --log-dir PATH
  --image REF              managed OCI image with the KumaBox guest agent
  --name NAME
  --snapshot NAME
  --storage SIZE
  --agent-timeout DURATION
  --sudo

The script restores the original VM in place and verifies VM/network identity,
guest memory state, and writable disk rollback. Only its named resources are
removed after success; failures are preserved for diagnosis.
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

[[ $(uname -s) == Linux ]] || { echo "native restore verification must run on Linux" >&2; exit 1; }
for command in jq tail; do
  command -v "$command" >/dev/null 2>&1 || { echo "$command is required" >&2; exit 1; }
done
if [[ $use_sudo == true ]]; then
  command -v sudo >/dev/null 2>&1 || { echo "--sudo requested but sudo is missing" >&2; exit 1; }
  kb_prefix=(sudo)
  tail_prefix=(sudo)
else
  kb_prefix=()
  tail_prefix=()
fi

step() { printf '\n==> %s\n' "$1"; }
kb() {
  "${kb_prefix[@]}" "$kumabox" \
    --root-dir "$root_dir" --run-dir "$run_dir" --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor" --qemu-img-bin "$qemu_img" "$@"
}

stop_tail() {
  if [[ -n $tail_pid ]]; then
    kill "$tail_pid" >/dev/null 2>&1 || true
    wait "$tail_pid" >/dev/null 2>&1 || true
    tail_pid=
  fi
}

clean_named_state() {
  kb delete "$name" --force >/dev/null 2>&1 || true
  kb snapshot rm "$snapshot_name" >/dev/null 2>&1 || true
}

on_exit() {
  stop_tail
  if [[ $success == true ]]; then
    clean_named_state
    return
  fi
  step "preserving failed native restore state"
  [[ -z $vm_id ]] || kb inspect "$vm_id" --json 2>/dev/null || true
  [[ -z $snapshot_id ]] || kb snapshot inspect "$snapshot_id" --json 2>/dev/null || true
  printf 'state: root_dir=%s run_dir=%s log_dir=%s\n' "$root_dir" "$run_dir" "$log_dir"
}
trap on_exit EXIT

step "clean previous named verification resources"
clean_named_state

step "environment checks"
scripts/linux/env-check.sh \
  --kumabox "$kumabox" --cloud-hypervisor "$cloud_hypervisor" --qemu-img "$qemu_img" --strict

step "inspect managed agent image"
kb image inspect "$image" --json | jq '{id, name, source, boot, oci}'

step "run source VM"
vm_json=$(kb run "$image" --name "$name" --network none --storage "$storage")
printf '%s\n' "$vm_json" | jq '{id, name, state, observedState, pid, vsockSocket, storageConfigs}'
vm_id=$(jq -r '.id' <<<"$vm_json")
log_path=$(jq -r '.logDir + "/console.log"' <<<"$vm_json")

step "tail guest console while waiting for agent"
"${tail_prefix[@]}" tail -n 0 -F "$log_path" &
tail_pid=$!
kb agent ping "$vm_id" --timeout "$agent_timeout" | jq .
stop_tail

step "create guest memory and disk markers"
process_pid=$(kb exec "$vm_id" -- sh -c 'nohup sh -c '\''while :; do date +%s%N > /run/kumabox-restore-heartbeat; sleep 1; done'\'' >/dev/null 2>&1 & echo $!')
kb exec "$vm_id" -- sh -c 'printf before-snapshot > /var/tmp/kumabox-restore-disk-marker'
printf 'state: guest_process_pid=%s\n' "$process_pid"
kb exec "$vm_id" -- sh -c "kill -0 $process_pid && test -s /run/kumabox-restore-heartbeat"

step "capture running snapshot"
snapshot_json=$(kb snapshot create "$vm_id" --name "$snapshot_name" --type running --consistent crash)
printf '%s\n' "$snapshot_json" | jq .
snapshot_id=$(jq -r '.id' <<<"$snapshot_json")

step "record identity that restore must preserve"
before_json=$(kb inspect "$vm_id" --json)
before_identity=$(jq -c '{id,name,network,networks,networkConfigs}' <<<"$before_json")
printf '%s\n' "$before_identity" | jq .

step "mutate disk and destroy the snapshotted process"
kb exec "$vm_id" -- sh -c 'printf after-snapshot > /var/tmp/kumabox-restore-disk-marker'
kb exec "$vm_id" -- kill "$process_pid"
if kb exec "$vm_id" -- kill -0 "$process_pid" >/dev/null 2>&1; then
  echo "guest process is still alive before restore" >&2
  exit 1
fi

step "restore snapshot into the original VM"
restore_json=$(kb restore "$vm_id" "$snapshot_id" --restore-mode copy)
printf '%s\n' "$restore_json" | jq '{id, name, state, observedState, pid, restore, networkConfigs}'
jq -e '.id == $id and .state == "running" and .observedState == "RUNNING" and (.restore == null)' --arg id "$vm_id" <<<"$restore_json" >/dev/null

step "tail guest console while restored agent reconnects"
"${tail_prefix[@]}" tail -n 0 -F "$log_path" &
tail_pid=$!
kb agent ping "$vm_id" --timeout "$agent_timeout" | jq .
stop_tail

step "prove memory and writable disk returned to snapshot point"
kb exec "$vm_id" -- sh -c "kill -0 $process_pid && test -s /run/kumabox-restore-heartbeat"
disk_marker=$(kb exec "$vm_id" -- cat /var/tmp/kumabox-restore-disk-marker)
printf 'state: restored_disk_marker=%s restored_process_pid=%s\n' "$disk_marker" "$process_pid"
[[ $disk_marker == before-snapshot ]] || { echo "writable disk did not roll back" >&2; exit 1; }

step "prove VM and network identity did not change"
after_json=$(kb inspect "$vm_id" --json)
after_identity=$(jq -c '{id,name,network,networks,networkConfigs}' <<<"$after_json")
jq -e --argjson before "$before_identity" '$before == {id,name,network,networks,networkConfigs}' <<<"$after_json" >/dev/null
printf '%s\n' "$after_identity" | jq .

step "remove verification resources"
kb delete "$vm_id" --force | jq '{id, name, state}'
kb snapshot rm "$snapshot_id" | jq .
vm_id=
snapshot_id=

success=true
echo "P5 native in-place restore verification passed"
