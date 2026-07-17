#!/usr/bin/env bash
set -euo pipefail

kumabox=./bin/kumabox
cloud_hypervisor=cloud-hypervisor
qemu_img=qemu-img
root_dir=/tmp/kumabox-p0/data
run_dir=/tmp/kumabox-p0/run
log_dir=/tmp/kumabox-p0/logs
image=p3-agent-image-v3
name=p2-real-cni
network_name=kumabox-e2e
bridge=kb-cni-e2e0
subnet=10.89.0.0/24
gateway=10.89.0.1
cni_bin_dir=/opt/cni/bin
storage=64M
timeout=180
external_target=
use_sudo=false
passed=false

usage() {
  cat <<'EOF'
Usage: scripts/linux/p2/verify-real-cni-e2e.sh [options]

Runs a managed OCI VM through KumaBox's real CNI provider. The test invokes
the installed bridge and host-local plugins, validates the CNI allocation,
netns/TAP/tc datapath, guest address and route, host-to-guest reachability,
guest-to-gateway reachability, and CNI DEL cleanup.

  --kumabox PATH
  --cloud-hypervisor PATH
  --qemu-img PATH
  --root-dir PATH
  --run-dir PATH
  --log-dir PATH
  --image NAME              existing managed OCI image
  --name NAME               verification VM name
  --cni-bin-dir PATH        defaults to /opt/cni/bin
  --network-name NAME       isolated CNI network name
  --bridge NAME             isolated CNI bridge name
  --subnet CIDR             defaults to 10.89.0.0/24
  --gateway IP              defaults to 10.89.0.1
  --storage SIZE            defaults to 64M
  --timeout SECONDS         defaults to 180
  --external-target IP      additionally require guest ping to this address
  --sudo

The script never removes the root, run, or log directory. On failure it keeps
the VM and CNI resources for inspection; rerunning cleans the named resources.
EOF
}

require_value() {
  [[ -n ${2:-} ]] || { echo "$1 requires a value" >&2; exit 2; }
}

while (($#)); do
  case "$1" in
    --kumabox) require_value "$1" "${2:-}"; kumabox=$2; shift 2 ;;
    --cloud-hypervisor) require_value "$1" "${2:-}"; cloud_hypervisor=$2; shift 2 ;;
    --qemu-img) require_value "$1" "${2:-}"; qemu_img=$2; shift 2 ;;
    --root-dir) require_value "$1" "${2:-}"; root_dir=$2; shift 2 ;;
    --run-dir) require_value "$1" "${2:-}"; run_dir=$2; shift 2 ;;
    --log-dir) require_value "$1" "${2:-}"; log_dir=$2; shift 2 ;;
    --image) require_value "$1" "${2:-}"; image=$2; shift 2 ;;
    --name) require_value "$1" "${2:-}"; name=$2; shift 2 ;;
    --cni-bin-dir) require_value "$1" "${2:-}"; cni_bin_dir=$2; shift 2 ;;
    --network-name) require_value "$1" "${2:-}"; network_name=$2; shift 2 ;;
    --bridge) require_value "$1" "${2:-}"; bridge=$2; shift 2 ;;
    --subnet) require_value "$1" "${2:-}"; subnet=$2; shift 2 ;;
    --gateway) require_value "$1" "${2:-}"; gateway=$2; shift 2 ;;
    --storage) require_value "$1" "${2:-}"; storage=$2; shift 2 ;;
    --timeout) require_value "$1" "${2:-}"; timeout=$2; shift 2 ;;
    --external-target) require_value "$1" "${2:-}"; external_target=$2; shift 2 ;;
    --sudo) use_sudo=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ $(uname -s) == Linux ]] || { echo "real CNI E2E requires Linux" >&2; exit 1; }
[[ $timeout =~ ^[1-9][0-9]*$ ]] || { echo "--timeout must be positive" >&2; exit 2; }
[[ ${#bridge} -le 15 ]] || { echo "--bridge exceeds Linux's 15-byte interface-name limit" >&2; exit 2; }

for command in jq ip tc ping "$cloud_hypervisor" "$qemu_img"; do
  command -v "$command" >/dev/null 2>&1 || { echo "required command not found: $command" >&2; exit 1; }
done
for plugin in bridge host-local loopback; do
  [[ -x $cni_bin_dir/$plugin ]] || { echo "CNI plugin is missing or not executable: $cni_bin_dir/$plugin" >&2; exit 1; }
done
[[ -x $kumabox ]] || { echo "kumabox is not executable: $kumabox" >&2; exit 1; }

if [[ $use_sudo == true ]]; then
  command -v sudo >/dev/null 2>&1 || { echo "sudo is required by --sudo" >&2; exit 1; }
  sudo -v
  kb_prefix=(sudo)
  privileged=(sudo)
else
  kb_prefix=()
  privileged=()
fi

work_dir=$run_dir/cni-e2e-$name
config_file=$work_dir/kumabox.toml
cni_conf_dir=$work_dir/net.d
cni_conf=$cni_conf_dir/10-$network_name.conflist

kb() {
  "${kb_prefix[@]}" "$kumabox" --config "$config_file" "$@"
}

section() {
  printf '\n==> %s\n' "$1"
}

install_text() {
  local destination=$1 content=$2 temporary
  temporary=$(mktemp)
  printf '%s\n' "$content" >"$temporary"
  "${privileged[@]}" install -m 0644 "$temporary" "$destination"
  rm -f "$temporary"
}

hydrate_failure_context() {
  local inspect_json
  inspect_json=$(kb inspect "$name" --json 2>/dev/null) || return 0
  vm_id=${vm_id:-$(jq -r '.id // empty' <<<"$inspect_json")}
  vm_log_dir=${vm_log_dir:-$(jq -r '.logDir // empty' <<<"$inspect_json")}
  vm_config=${vm_config:-$(jq -r '.config // empty' <<<"$inspect_json")}
  vm_pid=${vm_pid:-$(jq -r '.pid // empty' <<<"$inspect_json")}
  tap=${tap:-$(jq -r '.networkConfigs[0].tap // empty' <<<"$inspect_json")}
  netns_path=${netns_path:-$(jq -r '.networkConfigs[0].netnsPath // empty' <<<"$inspect_json")}
}

print_failure_context() {
  set +e
  hydrate_failure_context
  section "failure context: VM"
  kb inspect "$name" --json 2>/dev/null | jq . || true
  section "failure context: KumaBox network"
  kb network inspect "$name" --json 2>/dev/null | jq . || true
  section "failure context: host bridge"
  "${privileged[@]}" ip -d link show "$bridge" 2>/dev/null || true
  "${privileged[@]}" ip -4 address show "$bridge" 2>/dev/null || true
  if [[ -n ${vm_config:-} ]]; then
    section "failure context: rendered VMM config"
    "${privileged[@]}" jq . "$vm_config" 2>/dev/null || true
  fi
  if [[ -n ${vm_pid:-} ]]; then
    section "failure context: VMM process"
    "${privileged[@]}" ps -o pid,ppid,state,etimes,args -p "$vm_pid" 2>/dev/null || true
  fi
  if [[ -n ${netns_path:-} ]]; then
    section "failure context: CNI namespace"
    "${privileged[@]}" ip netns exec "$(basename "$netns_path")" ip -d link 2>/dev/null || true
    "${privileged[@]}" ip netns exec "$(basename "$netns_path")" ip address 2>/dev/null || true
    "${privileged[@]}" ip netns exec "$(basename "$netns_path")" ip tuntap show 2>/dev/null || true
    "${privileged[@]}" ip netns exec "$(basename "$netns_path")" tc filter show dev eth0 ingress 2>/dev/null || true
    [[ -z ${tap:-} ]] || "${privileged[@]}" ip netns exec "$(basename "$netns_path")" tc filter show dev "$tap" ingress 2>/dev/null || true
  fi
  if [[ -n ${vm_log_dir:-} ]]; then
    section "failure context: console tail"
    "${privileged[@]}" tail -n 120 "$vm_log_dir/console.log" 2>/dev/null || true
    section "failure context: VMM stderr"
    "${privileged[@]}" tail -n 120 "$vm_log_dir/cloud-hypervisor.stderr.log" 2>/dev/null || true
    section "failure context: VMM stdout"
    "${privileged[@]}" tail -n 120 "$vm_log_dir/cloud-hypervisor.stdout.log" 2>/dev/null || true
  fi
}

cleanup() {
  local status=$?
  set +e
  if [[ $passed != true ]]; then
    printf '\nreal CNI E2E failed with status %s; state preserved\n' "$status" >&2
    printf 'state: vm=%s config=%s root_dir=%s run_dir=%s log_dir=%s\n' \
      "$name" "$config_file" "$root_dir" "$run_dir" "$log_dir" >&2
    print_failure_context
    return
  fi
  "${privileged[@]}" ip link delete "$bridge" >/dev/null 2>&1 || true
  "${privileged[@]}" rm -rf "$work_dir"
}
trap cleanup EXIT

section "clean previous named verification resources"
if [[ -f $config_file ]]; then
  if kb inspect "$name" --json >/dev/null 2>&1; then
    kb delete "$name" --force >/dev/null
  fi
fi
"${privileged[@]}" ip link delete "$bridge" >/dev/null 2>&1 || true
"${privileged[@]}" rm -rf "$work_dir"
"${privileged[@]}" install -d -m 0755 "$work_dir" "$cni_conf_dir" "$root_dir" "$run_dir" "$log_dir"

install_text "$config_file" "[runtime]
root_dir = \"$root_dir\"
run_dir = \"$run_dir\"
log_dir = \"$log_dir\"

[backend.cloud_hypervisor]
binary = \"$cloud_hypervisor\"
api_socket_timeout_ms = 5000
stop_timeout_ms = 10000

[storage]
qemu_img_binary = \"$qemu_img\"

[network]
mode = \"cni\"
default = \"$network_name\"
bridge = \"$bridge\"
cidr = \"$subnet\"
gateway = \"$gateway\"
dns = [\"1.1.1.1\", \"8.8.8.8\"]
tap_prefix = \"kbcni\"
nat_backend = \"none\"
cni_config_dir = \"$cni_conf_dir\"
cni_bin_dir = \"$cni_bin_dir\""

install_text "$cni_conf" "{
  \"cniVersion\": \"1.0.0\",
  \"name\": \"$network_name\",
  \"plugins\": [
    {
      \"type\": \"bridge\",
      \"bridge\": \"$bridge\",
      \"isGateway\": true,
      \"ipMasq\": true,
      \"hairpinMode\": true,
      \"ipam\": {
        \"type\": \"host-local\",
        \"ranges\": [[{\"subnet\": \"$subnet\", \"gateway\": \"$gateway\"}]],
        \"routes\": [{\"dst\": \"0.0.0.0/0\"}]
      },
      \"dns\": {\"nameservers\": [\"1.1.1.1\", \"8.8.8.8\"]}
    }
  ]
}"

section "inspect managed OCI image"
kb image inspect "$image" --json | jq '{id,name,boot,agentInjection:.oci.agentInjection}'

section "run VM through real bridge and host-local plugins"
run_json=$(kb run "$image" --name "$name" --network "cni:$network_name" --storage "$storage")
printf '%s\n' "$run_json" | jq .
vm_id=$(jq -r '.id' <<<"$run_json")
tap=$(jq -r '.networkConfigs[0].tap' <<<"$run_json")
netns_path=$(jq -r '.networkConfigs[0].netnsPath' <<<"$run_json")
guest_ip=$(jq -r '.networkConfigs[0].network.ip' <<<"$run_json")
guest_prefix=$(jq -r '.networkConfigs[0].network.prefix' <<<"$run_json")
vm_log_dir=$(jq -r '.logDir' <<<"$run_json")
[[ -n $vm_id && $vm_id != null && -n $tap && $tap != null && -n $netns_path && $netns_path != null && -n $guest_ip && $guest_ip != null && $guest_prefix =~ ^[0-9]+$ ]] || {
  echo "run result is missing CNI identity" >&2
  exit 1
}

section "verify provider state and Linux datapath"
network_json=$(kb network inspect "$name" --json)
printf '%s\n' "$network_json" | jq .
jq -e --arg network "cni:$network_name" --arg address "$guest_ip/$guest_prefix" '
  (.interfaces | length) == 1 and
  .interfaces[0].provider == "cni" and
  .interfaces[0].network == $network and
  (.interfaces[0].ips | index($address)) != null and
  (.drift | length) == 0
' <<<"$network_json" >/dev/null

ns_name=$(basename "$netns_path")
"${privileged[@]}" ip -d link show "$bridge"
"${privileged[@]}" ip netns exec "$ns_name" ip -d link show eth0
"${privileged[@]}" ip netns exec "$ns_name" ip -d link show "$tap"
eth0_filters=$("${privileged[@]}" ip netns exec "$ns_name" tc filter show dev eth0 ingress)
tap_filters=$("${privileged[@]}" ip netns exec "$ns_name" tc filter show dev "$tap" ingress)
printf '%s\n%s\n' "$eth0_filters" "$tap_filters"
[[ $eth0_filters == *mirred* && $tap_filters == *mirred* ]] || {
  echo "CNI tc redirect filters are incomplete" >&2
  exit 1
}

section "wait for guest agent and inspect guest network"
kb agent ping "$name" --timeout "${timeout}s" | jq .
guest_network=$(kb exec "$name" -- sh -c 'ip -brief link; ip -brief address; ip route')
printf '%s\n' "$guest_network"
grep -Fq "$guest_ip/$guest_prefix" <<<"$guest_network" || { echo "guest is missing CNI address $guest_ip/$guest_prefix" >&2; exit 1; }
grep -Fq "default via $gateway" <<<"$guest_network" || { echo "guest is missing CNI default route" >&2; exit 1; }

section "verify bidirectional reachability"
ping -c 1 -W 3 "$guest_ip"
kb exec "$name" -- ping -c 1 -W 3 "$gateway"
if [[ -n $external_target ]]; then
  kb exec "$name" -- ping -c 1 -W 3 "$external_target"
fi

section "delete VM and verify CNI DEL cleanup"
kb delete "$name" --force | jq .
[[ ! -e $netns_path ]] || { echo "CNI namespace remains after delete: $netns_path" >&2; exit 1; }
if kb network inspect "$name" --json >/dev/null 2>&1; then
  echo "KumaBox network record remains after delete" >&2
  exit 1
fi

passed=true
echo "KumaBox real CNI E2E verification passed"
