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
ping_timeout=60
ping_interval=2
use_sudo=false
success=false
source_id=
clone_id=
snapshot_id=
source_ip=
clone_ip=
source_tap=
clone_tap=
active_step="initialization"
failure_status=
failure_line=
failure_command=
failure_stage=

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
  --ping-timeout SECONDS
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
    --ping-timeout) ping_timeout=$2; shift 2 ;;
    --sudo) use_sudo=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ $(uname -s) == Linux ]] || { echo "native clone verification must run on Linux" >&2; exit 1; }
[[ $ping_timeout =~ ^[1-9][0-9]*$ ]] || { echo "--ping-timeout must be a positive integer" >&2; exit 2; }
for command in ip jq ping timeout; do
  command -v "$command" >/dev/null 2>&1 || { echo "$command is required" >&2; exit 1; }
done
if [[ $use_sudo == true ]]; then
  command -v sudo >/dev/null 2>&1 || { echo "--sudo requested but sudo is missing" >&2; exit 1; }
  kb_prefix=(sudo)
else
  kb_prefix=()
fi

step() {
  active_step=$1
  printf '\n==> %s\n' "$1"
}
record_error() {
  failure_status=$1
  failure_line=$2
  failure_command=$3
  failure_stage=$active_step
}
print_failure_summary() {
  printf 'failure: stage=%q status=%s line=%s command=%q\n' \
    "${failure_stage:-$active_step}" "${failure_status:-unknown}" "${failure_line:-unknown}" "${failure_command:-unknown}"
}
kb() {
  "${kb_prefix[@]}" "$kumabox" \
    --root-dir "$root_dir" --run-dir "$run_dir" --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor" --qemu-img-bin "$qemu_img" "$@"
}
wait_for_ping() {
  local guest=$1
  local address=$2
  local started now elapsed attempt

  started=$(date +%s)
  attempt=1
  while true; do
    if ping -n -c 1 -W 2 "$address" >/dev/null 2>&1; then
      now=$(date +%s)
      elapsed=$((now - started))
      printf 'pass: host can ping %s guest at %s after %ss\n' "$guest" "$address" "$elapsed"
      return 0
    fi

    now=$(date +%s)
    elapsed=$((now - started))
    if ((elapsed >= ping_timeout)); then
      printf 'host could not ping %s guest at %s after %ss\n' "$guest" "$address" "$elapsed" >&2
      return 1
    fi
    printf 'state: %s ping attempt %s failed after %ss; waiting %ss\n' \
      "$guest" "$attempt" "$elapsed" "$ping_interval"
    sleep "$ping_interval"
    attempt=$((attempt + 1))
  done
}
print_host_network_context() {
  local guest=$1
  local tap=$2
  local address=$3

  [[ -n $tap ]] || return 0
  step "failure context: $guest host TAP"
  ip -d link show "$tap" 2>/dev/null || true
  if command -v bridge >/dev/null 2>&1; then
    bridge link show dev "$tap" 2>/dev/null || true
  fi
  if [[ -n $address ]]; then
    printf '%s\n' "neighbor lookup for $address"
    ip neigh show "$address" 2>/dev/null || true
  fi
}
clean_named_state() {
  kb delete "$clone_name" --force >/dev/null 2>&1 || true
  kb delete "$source_name" --force >/dev/null 2>&1 || true
  kb snapshot rm "$snapshot_name" >/dev/null 2>&1 || true
}
on_exit() {
  local exit_status=$?
  if [[ $success == true ]]; then
    clean_named_state
    return
  fi
  if [[ -z $failure_status ]]; then
    failure_status=$exit_status
  fi
  print_failure_summary >&2
  step "preserving failed native clone state"
  [[ -z $source_id ]] || kb inspect "$source_id" --json 2>/dev/null || true
  [[ -z $clone_id ]] || kb inspect "$clone_id" --json 2>/dev/null || true
  [[ -z $snapshot_id ]] || kb snapshot inspect "$snapshot_id" --json 2>/dev/null || true
  if [[ -n $clone_id ]]; then
    step "failure context: clone guest agent"
    kb agent ping "$clone_id" --timeout 5s 2>/dev/null | jq . || true

    step "failure context: clone network"
    kb network inspect "$clone_id" --json 2>/dev/null | jq . || true

    step "failure context: clone guest link, address, and route"
    timeout 10s "${kb_prefix[@]}" "$kumabox" \
      --root-dir "$root_dir" --run-dir "$run_dir" --log-dir "$log_dir" \
      --cloud-hypervisor-bin "$cloud_hypervisor" --qemu-img-bin "$qemu_img" \
      exec "$clone_id" -- sh -c 'ip -brief link; ip -brief address; ip route' 2>/dev/null || true

    print_host_network_context clone "$clone_tap" "$clone_ip"

    step "failure context: clone console tail"
    kb logs "$clone_id" --source console --tail 120 2>/dev/null || true

    step "failure context: clone VMM stderr"
    kb logs "$clone_id" --source stderr --tail 80 2>/dev/null || true
  fi
  if [[ -n $source_id ]]; then
    step "failure context: source guest agent"
    kb agent ping "$source_id" --timeout 5s 2>/dev/null | jq . || true

    step "failure context: source network"
    kb network inspect "$source_id" --json 2>/dev/null | jq . || true

    print_host_network_context source "$source_tap" "$source_ip"

    step "failure context: source console tail"
    kb logs "$source_id" --source console --tail 120 2>/dev/null || true

    step "failure context: source VMM stderr"
    kb logs "$source_id" --source stderr --tail 80 2>/dev/null || true
  fi
  print_failure_summary >&2
  printf 'state: root_dir=%s run_dir=%s log_dir=%s\n' "$root_dir" "$run_dir" "$log_dir"
}
trap 'record_error "$?" "$LINENO" "$BASH_COMMAND"' ERR
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
source_agent=$(kb agent ping "$source_id" --timeout "$agent_timeout")
printf '%s\n' "$source_agent" | jq .
if ! jq -e '(.agent.capabilities // []) | index("identity") != null' >/dev/null <<<"$source_agent"; then
  agent_version=$(jq -r '.agent.version // "unknown"' <<<"$source_agent")
  agent_capabilities=$(jq -c '.agent.capabilities // []' <<<"$source_agent")
  echo "guest agent $agent_version does not support native clone identity (capabilities=$agent_capabilities)" >&2
  echo "rebuild p3-agent-image with scripts/linux/p3/verify-oci-agent-transport.sh before running this verification" >&2
  exit 1
fi
echo "pass: guest agent advertises identity capability"

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
source_ip=$(jq -r '.networkConfigs[0].network.ip' <<<"$source_after")
clone_ip=$(jq -r '.networkConfigs[0].network.ip' <<<"$clone_after")
source_tap=$(jq -r '.networkConfigs[0].tap' <<<"$source_after")
clone_tap=$(jq -r '.networkConfigs[0].tap' <<<"$clone_after")
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

step "inspect source and clone guest network state"
printf '%s\n' "source guest ($source_ip):"
kb exec "$source_id" -- sh -c 'ip -brief link; ip -brief address; ip route'
printf '%s\n' "clone guest ($clone_ip):"
kb exec "$clone_id" -- sh -c 'ip -brief link; ip -brief address; ip route'

step "prove both guests are independently reachable"
wait_for_ping source "$source_ip"
wait_for_ping clone "$clone_ip"
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
