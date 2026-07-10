#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
cloud_hypervisor_path="cloud-hypervisor"
qemu_img_path="qemu-img"
root_dir="/tmp/kumabox-p0/data"
run_dir="/tmp/kumabox-p0/run"
log_dir="/tmp/kumabox-p0/logs"
name="p2-cni"
root_disk=""
firmware=""
use_sudo=0

usage() {
  cat <<'USAGE'
Usage: scripts/linux/p2/verify-cni-provider.sh --root-disk PATH --firmware PATH [options]

Verifies P2-08 CNI provider state handling and Linux datapath rendering with a
mock CNI plugin. The script checks CNI ADD, per-VM netns creation, TAP creation,
tc redirect filters, VM networkConfigs, CNI DEL cleanup, and pending cleanup on
DEL failure. It does not start a VM.

Options:
  --kumabox PATH             kumabox binary path
  --cloud-hypervisor PATH    cloud-hypervisor binary or command
  --qemu-img PATH            qemu-img binary or command
  --root-dir PATH            persistent state directory
  --run-dir PATH             runtime directory
  --log-dir PATH             log directory
  --name NAME                VM name prefix
  --root-disk PATH           root disk fixture path
  --firmware PATH            UEFI firmware path
  --sudo                     clean/write state through sudo
USAGE
}

require_value() {
  local flag="$1"
  local value="${2:-}"
  if [[ -z "$value" ]]; then
    echo "$flag requires a value" >&2
    exit 2
  fi
}

section() {
  printf '\n==> %s\n' "$1"
}

write_file() {
  local path="$1"
  local content="$2"
  local tmp
  tmp="$(mktemp)"
  printf '%s\n' "$content" >"$tmp"
  "${copy_cmd[@]}" "$tmp" "$path"
  rm -f "$tmp"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --kumabox)
      require_value "$1" "${2:-}"
      kumabox_path="$2"
      shift 2
      ;;
    --cloud-hypervisor)
      require_value "$1" "${2:-}"
      cloud_hypervisor_path="$2"
      shift 2
      ;;
    --qemu-img)
      require_value "$1" "${2:-}"
      qemu_img_path="$2"
      shift 2
      ;;
    --root-dir)
      require_value "$1" "${2:-}"
      root_dir="$2"
      shift 2
      ;;
    --run-dir)
      require_value "$1" "${2:-}"
      run_dir="$2"
      shift 2
      ;;
    --log-dir)
      require_value "$1" "${2:-}"
      log_dir="$2"
      shift 2
      ;;
    --name)
      require_value "$1" "${2:-}"
      name="$2"
      shift 2
      ;;
    --root-disk)
      require_value "$1" "${2:-}"
      root_disk="$2"
      shift 2
      ;;
    --firmware)
      require_value "$1" "${2:-}"
      firmware="$2"
      shift 2
      ;;
    --sudo)
      use_sudo=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ -z "$root_disk" || -z "$firmware" ]]; then
  usage >&2
  exit 2
fi

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "verify-cni-provider must run on Linux" >&2
  exit 1
fi

for bin in jq; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "$bin is required" >&2
    exit 1
  fi
done

if ! command -v ip >/dev/null 2>&1; then
  echo "ip command is required" >&2
  exit 1
fi

if [[ "$use_sudo" -eq 1 ]]; then
  if ! command -v sudo >/dev/null 2>&1; then
    echo "--sudo requested but sudo is missing" >&2
    exit 1
  fi
  kumabox_cmd=(sudo "$kumabox_path")
  remove_cmd=(sudo rm -rf)
  mkdir_cmd=(sudo mkdir -p)
  chmod_cmd=(sudo chmod)
  touch_cmd=(sudo touch)
  rm_cmd=(sudo rm -f)
  cat_cmd=(sudo cat)
  copy_cmd=(sudo cp)
  ip_cmd=(sudo ip)
else
  kumabox_cmd=("$kumabox_path")
  remove_cmd=(rm -rf)
  mkdir_cmd=(mkdir -p)
  chmod_cmd=(chmod)
  touch_cmd=(touch)
  rm_cmd=(rm -f)
  cat_cmd=(cat)
  copy_cmd=(cp)
  ip_cmd=(ip)
fi

cni_conf_dir="$root_dir/cni/net.d"
cni_bin_dir="$root_dir/cni/bin"
cni_log="$root_dir/cni/plugin.log"
cni_fail_file="$root_dir/cni/fail-del"
config_path="$root_dir/kumabox-cni.toml"

cleanup() {
  set +e
  "${rm_cmd[@]}" "$cni_fail_file" >/dev/null 2>&1
  "${kumabox_cmd[@]}" --config "$config_path" delete "${name}-ok" --force >/dev/null 2>&1
  "${kumabox_cmd[@]}" --config "$config_path" delete "${name}-pending" --force >/dev/null 2>&1
  "${remove_cmd[@]}" "$root_dir" "$run_dir" "$log_dir"
}
trap cleanup EXIT

section "clean previous P2-08 state"
"${remove_cmd[@]}" "$root_dir" "$run_dir" "$log_dir"
"${mkdir_cmd[@]}" "$root_dir" "$run_dir" "$log_dir" "$cni_conf_dir" "$cni_bin_dir"

section "environment checks"
scripts/linux/env-check.sh \
  --kumabox "$kumabox_path" \
  --cloud-hypervisor "$cloud_hypervisor_path" \
  --qemu-img "$qemu_img_path" \
  --strict

section "write mock CNI config and plugin"
write_file "$config_path" "[runtime]
root_dir = \"$root_dir\"
run_dir = \"$run_dir\"
log_dir = \"$log_dir\"

[backend.cloud_hypervisor]
binary = \"$cloud_hypervisor_path\"
api_socket_timeout_ms = 5000
stop_timeout_ms = 10000

[network]
mode = \"cni\"
default = \"default\"
bridge = \"kumabox0\"
cidr = \"10.88.0.0/16\"
gateway = \"10.88.0.1\"
dns = [\"1.1.1.1\", \"8.8.8.8\"]
tap_prefix = \"kbtap\"
nat_backend = \"none\"
cni_config_dir = \"$cni_conf_dir\"
cni_bin_dir = \"$cni_bin_dir\""

write_file "$cni_conf_dir/default.conf" '{
  "cniVersion": "1.0.0",
  "name": "default",
  "type": "kumabox-mock"
}'

write_file "$cni_bin_dir/kumabox-mock" "#!/bin/sh
set -eu
cat >/dev/null
printf '%s %s %s %s\n' \"\$CNI_COMMAND\" \"\$CNI_CONTAINERID\" \"\$CNI_IFNAME\" \"\$CNI_NETNS\" >> \"$cni_log\"
ns_name=\$(basename \"\$CNI_NETNS\")
if [ \"\$CNI_COMMAND\" = \"ADD\" ]; then
  ip netns exec \"\$ns_name\" ip link del \"\$CNI_IFNAME\" >/dev/null 2>&1 || true
  ip netns exec \"\$ns_name\" ip link add \"\$CNI_IFNAME\" type dummy
  ip netns exec \"\$ns_name\" ip link set dev \"\$CNI_IFNAME\" address 5a:00:00:00:00:66
  ip netns exec \"\$ns_name\" ip addr add 10.244.0.9/24 dev \"\$CNI_IFNAME\"
  ip netns exec \"\$ns_name\" ip link set dev \"\$CNI_IFNAME\" up
  printf '{\"cniVersion\":\"1.0.0\",\"interfaces\":[{\"name\":\"%s\",\"mac\":\"5a:00:00:00:00:66\",\"sandbox\":\"%s\"}],\"ips\":[{\"address\":\"10.244.0.9/24\",\"gateway\":\"10.244.0.1\",\"interface\":0}],\"dns\":{\"nameservers\":[\"1.1.1.1\"]}}\n' \"\$CNI_IFNAME\" \"\$CNI_NETNS\"
  exit 0
fi
if [ \"\$CNI_COMMAND\" = \"DEL\" ]; then
  if [ -f \"$cni_fail_file\" ]; then
    echo forced cni del failure >&2
    exit 7
  fi
  ip netns exec \"\$ns_name\" ip link del \"\$CNI_IFNAME\" >/dev/null 2>&1 || true
  exit 0
fi
exit 0"
"${chmod_cmd[@]}" +x "$cni_bin_dir/kumabox-mock"

section "create VM with --network cni:default"
created_json="$("${kumabox_cmd[@]}" \
  --config "$config_path" \
  create \
  --name "${name}-ok" \
  --root-disk "$root_disk" \
  --firmware "$firmware" \
  --network cni:default)"
printf '%s\n' "$created_json"
vm_id="$(printf '%s' "$created_json" | jq -r '.id')"
tap="$(printf '%s' "$created_json" | jq -r '.networkConfigs[0].tap')"
if_name="$(printf '%s' "$created_json" | jq -r '.networkConfigs[0].ifName')"
netns_path="$(printf '%s' "$created_json" | jq -r '.networkConfigs[0].netnsPath')"
if [[ "$(printf '%s' "$created_json" | jq -r '.networkConfigs[0].backend')" != "cni" ]]; then
  echo "VM network config backend is not cni" >&2
  exit 1
fi
if [[ "$(printf '%s' "$created_json" | jq -r '.networkConfigs[0].network.ip')" != "10.244.0.9" ]]; then
  echo "CNI result IP was not copied into VM record" >&2
  exit 1
fi
if [[ "$if_name" != "eth0" ]]; then
  echo "VM network config ifName is not eth0: $if_name" >&2
  exit 1
fi
if [[ "$netns_path" != "/var/run/netns/$vm_id" ]]; then
  echo "VM network config netnsPath is unexpected: $netns_path" >&2
  exit 1
fi
printf 'state: cni vm=%s if=%s tap=%s netns=%s\n' "$vm_id" "$if_name" "$tap" "$netns_path"

section "netns links"
"${ip_cmd[@]}" netns exec "$vm_id" ip -d link show "$if_name"
"${ip_cmd[@]}" netns exec "$vm_id" ip -d link show "$tap"
if "${ip_cmd[@]}" netns exec "$vm_id" ip addr show "$if_name" | grep -q '10.244.0.9'; then
  echo "CNI address should have been flushed from host-side link" >&2
  exit 1
fi
printf 'state: CNI link address was flushed; guest IP is delivered through VM metadata\n'

section "tc redirect filters"
"${ip_cmd[@]}" netns exec "$vm_id" tc filter show dev "$if_name" ingress
"${ip_cmd[@]}" netns exec "$vm_id" tc filter show dev "$tap" ingress

section "network inspect"
network_json="$("${kumabox_cmd[@]}" --config "$config_path" network inspect "${name}-ok" --json)"
printf '%s\n' "$network_json"
if [[ "$(printf '%s' "$network_json" | jq -r '.interfaces[0].provider')" != "cni" ]]; then
  echo "provider index did not record cni provider" >&2
  exit 1
fi
if [[ "$(printf '%s' "$network_json" | jq -r '.drift | length')" != "0" ]]; then
  echo "network inspect reported drift for CNI create" >&2
  exit 1
fi

section "rendered Cloud Hypervisor net config"
config_file="$(printf '%s' "$created_json" | jq -r '.config')"
"${cat_cmd[@]}" "$config_file" | jq '{netnsPath, nets}'
if [[ "$("${cat_cmd[@]}" "$config_file" | jq -r '.netnsPath')" != "$netns_path" ]]; then
  echo "Cloud Hypervisor config netnsPath does not match VM network config" >&2
  exit 1
fi

section "delete VM and verify CNI DEL"
"${kumabox_cmd[@]}" --config "$config_path" delete "${name}-ok" --force
if ! "${cat_cmd[@]}" "$cni_log" | grep -q "DEL $vm_id eth0 /var/run/netns/$vm_id"; then
  echo "mock CNI plugin did not receive DEL" >&2
  "${cat_cmd[@]}" "$cni_log" || true
  exit 1
fi
if "${ip_cmd[@]}" netns list | grep -q "^$vm_id "; then
  echo "netns remains after successful CNI delete" >&2
  "${ip_cmd[@]}" netns list
  exit 1
fi
if [[ "$("${kumabox_cmd[@]}" --config "$config_path" network ls --json | jq 'length')" != "0" ]]; then
  echo "provider records remain after successful CNI delete" >&2
  exit 1
fi

section "CNI DEL failure marks cleanup pending"
pending_json="$("${kumabox_cmd[@]}" \
  --config "$config_path" \
  create \
  --name "${name}-pending" \
  --root-disk "$root_disk" \
  --firmware "$firmware" \
  --network cni:default)"
printf '%s\n' "$pending_json"
pending_id="$(printf '%s' "$pending_json" | jq -r '.id')"
"${touch_cmd[@]}" "$cni_fail_file"
set +e
"${kumabox_cmd[@]}" --config "$config_path" delete "${name}-pending" --force >/tmp/kumabox-p2-cni-delete.out 2>&1
delete_status=$?
set -e
cat /tmp/kumabox-p2-cni-delete.out
rm -f /tmp/kumabox-p2-cni-delete.out
if [[ "$delete_status" -eq 0 ]]; then
  echo "delete should fail when CNI DEL fails" >&2
  exit 1
fi
pending_records="$("${kumabox_cmd[@]}" --config "$config_path" network ls --json)"
printf '%s\n' "$pending_records"
if ! printf '%s' "$pending_records" | jq -e '.[] | select(.vmId == "'"$pending_id"'" and .cleanup.pending == true and (.cleanup.reason | contains("forced cni del failure")))' >/dev/null; then
  echo "CNI DEL failure did not mark cleanup pending" >&2
  exit 1
fi

section "cleanup pending VM after clearing failure"
"${rm_cmd[@]}" "$cni_fail_file"
"${kumabox_cmd[@]}" --config "$config_path" delete "${name}-pending" --force

echo "P2-08 CNI provider verification passed"
