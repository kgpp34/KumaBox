#!/usr/bin/env bash
set -euo pipefail

kumabox=./bin/kumabox
cloud_hypervisor=cloud-hypervisor
qemu_img=qemu-img
root_dir=/tmp/kumabox-p0/data
run_dir=/tmp/kumabox-p0/run
log_dir=/tmp/kumabox-p0/logs
image=p3-agent-image
source_name=p5-clone-source
clone_name=p5-clone-target
snapshot_name=p5-clone-native
storage=64M
agent_timeout=180s
use_sudo=false
success=false
source_id=
clone_id=
snapshot_id=

usage() {
  cat <<'EOF'
Usage: verify-native-clone.sh [options]

  --kumabox PATH
  --cloud-hypervisor PATH
  --qemu-img PATH
  --root-dir PATH
  --run-dir PATH
  --log-dir PATH
  --image REF              managed OCI image with the KumaBox guest agent
  --source-name NAME
  --clone-name NAME
  --snapshot NAME
  --storage SIZE
  --agent-timeout DURATION
  --sudo

Verifies native clone memory/disk continuity and proves that VM, hostname,
vsock, MAC, IP, TAP, and provider records are independent from the source.
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
    --source-name) source_name=$2; shift 2 ;;
    --clone-name) clone_name=$2; shift 2 ;;
    --snapshot) snapshot_name=$2; shift 2 ;;
    --storage) storage=$2; shift 2 ;;
    --agent-timeout) agent_timeout=$2; shift 2 ;;
    --sudo) use_sudo=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ $(uname -s) == Linux ]] || { echo "native clone verification must run on Linux" >&2; exit 1; }
for command in jq ping; do
  command -v "$command" >/dev/null 2>&1 || { echo "$command is required" >&2; exit 1; }
done
if [[ $use_sudo == true ]]; then
  command -v sudo >/dev/null 2>&1 || { echo "--sudo requested but sudo is missing" >&2; exit 1; }
  kb_prefix=(sudo)
else
  kb_prefix=()
fi

step() { printf '\n==> %s\n' "$1"; }
kb() {
  "${kb_prefix[@]}" "$kumabox" \
    --root-dir "$root_dir" --run-dir "$run_dir" --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor" --qemu-img-bin "$qemu_img" "$@"
}
clean_named_state() {
  kb delete "$clone_name" --force >/dev/null 2>&1 || true
  kb delete "$source_name" --force >/dev/null 2>&1 || true
  kb snapshot rm "$snapshot_name" >/dev/null 2>&1 || true
}
on_exit() {
  if [[ $success == true ]]; then
    clean_named_state
    return
  fi
  step "preserving failed native clone state"
  [[ -z $source_id ]] || kb inspect "$source_id" --json 2>/dev/null || true
  [[ -z $clone_id ]] || kb inspect "$clone_id" --json 2>/dev/null || true
  [[ -z $snapshot_id ]] || kb snapshot inspect "$snapshot_id" --json 2>/dev/null || true
  printf 'state: root_dir=%s run_dir=%s log_dir=%s\n' "$root_dir" "$run_dir" "$log_dir"
}
trap on_exit EXIT

step "clean previous named verification resources"
clean_named_state

step "environment checks"
scripts/linux/env-check.sh \
  --kumabox "$kumabox" --cloud-hypervisor "$cloud_hypervisor" --qemu-img "$qemu_img" --strict --network

step "run source VM with one independent network identity"
source_json=$(kb run "$image" --name "$source_name" --network default --storage "$storage")
printf '%s\n' "$source_json" | jq '{id,name,state,vsockSocket,networkConfigs}'
source_id=$(jq -r '.id' <<<"$source_json")
kb agent ping "$source_id" --timeout "$agent_timeout" | jq .

step "create state that must appear in the clone"
process_pid=$(kb exec "$source_id" -- sh -c 'nohup sh -c '\''while :; do date +%s%N > /run/kumabox-clone-heartbeat; sleep 1; done'\'' >/dev/null 2>&1 & echo $!')
kb exec "$source_id" -- sh -c 'printf snapshot-disk-state > /var/tmp/kumabox-clone-marker'
printf 'state: snapshotted_process_pid=%s\n' "$process_pid"

step "capture native snapshot"
snapshot_json=$(kb snapshot create "$source_id" --name "$snapshot_name" --type running --consistent crash)
printf '%s\n' "$snapshot_json" | jq .
snapshot_id=$(jq -r '.id' <<<"$snapshot_json")

step "clone with new storage, vsock, and provider network identity"
clone_json=$(kb clone "$snapshot_id" --name "$clone_name" --restore-mode copy)
printf '%s\n' "$clone_json" | jq '{id,name,state,observedState,pid,vsockSocket,storageConfigs,networkConfigs}'
clone_id=$(jq -r '.id' <<<"$clone_json")
kb agent ping "$clone_id" --timeout "$agent_timeout" | jq .

step "verify cloned guest memory, disk, and hostname"
kb exec "$clone_id" -- sh -c "kill -0 $process_pid && test -s /run/kumabox-clone-heartbeat"
marker=$(kb exec "$clone_id" -- cat /var/tmp/kumabox-clone-marker)
hostname=$(kb exec "$clone_id" -- uname -n)
printf 'state: clone_process_pid=%s marker=%s hostname=%s\n' "$process_pid" "$marker" "$hostname"
[[ $marker == snapshot-disk-state ]] || { echo "clone disk state mismatch" >&2; exit 1; }
[[ $hostname == "$clone_name" ]] || { echo "clone hostname was not reseeded" >&2; exit 1; }

step "compare source and clone identities"
source_after=$(kb inspect "$source_id" --json)
clone_after=$(kb inspect "$clone_id" --json)
jq -n -e --argjson source "$source_after" --argjson clone "$clone_after" '
  ($source.id != $clone.id) and
  ($source.name != $clone.name) and
  ($source.vsockSocket != $clone.vsockSocket) and
  (($source.networkConfigs | length) == ($clone.networkConfigs | length)) and
  ([range(0; $source.networkConfigs | length)] | all(. as $i |
    ($source.networkConfigs[$i].mac != $clone.networkConfigs[$i].mac) and
    ($source.networkConfigs[$i].tap != $clone.networkConfigs[$i].tap) and
    ($source.networkConfigs[$i].network.ip != $clone.networkConfigs[$i].network.ip)))
' >/dev/null
printf '%s\n' "$source_after" | jq '{id,name,state,observedState,vsockSocket,networkConfigs}'
printf '%s\n' "$clone_after" | jq '{id,name,state,observedState,vsockSocket,networkConfigs}'

step "prove both guests are independently reachable"
source_ip=$(jq -r '.networkConfigs[0].network.ip' <<<"$source_after")
clone_ip=$(jq -r '.networkConfigs[0].network.ip' <<<"$clone_after")
ping -c 1 -W 2 "$source_ip"
ping -c 1 -W 2 "$clone_ip"
kb exec "$source_id" -- sh -c "kill -0 $process_pid"

step "remove verification resources"
kb delete "$clone_id" --force | jq '{id,name,state}'
kb delete "$source_id" --force | jq '{id,name,state}'
kb snapshot rm "$snapshot_id" | jq .
clone_id=
source_id=
snapshot_id=

success=true
echo "P5 native clone identity and datapath verification passed"
