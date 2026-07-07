#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
cloud_hypervisor_path="cloud-hypervisor"
qemu_img_path="qemu-img"
root_dir="/tmp/kumabox-p0/data"
run_dir="/tmp/kumabox-p0/run"
log_dir="/tmp/kumabox-p0/logs"
name="p2-multinic"
root_disk=""
firmware=""
use_sudo=0

usage() {
  cat <<'USAGE'
Usage: scripts/linux/verify-multinic-model.sh --root-disk PATH --firmware PATH [options]

Verifies P2-09 multi-NIC network model without starting a VM. The script creates
a VM with two CNI network attachments and checks VM intent, provider records,
rendered Cloud Hypervisor nets, and delete cleanup.

Options:
  --kumabox PATH             kumabox binary path
  --cloud-hypervisor PATH    cloud-hypervisor binary or command
  --qemu-img PATH            qemu-img binary or command
  --root-dir PATH            persistent state directory
  --run-dir PATH             runtime directory
  --log-dir PATH             log directory
  --name NAME                VM name
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
  echo "verify-multinic-model must run on Linux" >&2
  exit 1
fi

for bin in jq ip; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "$bin is required" >&2
    exit 1
  fi
done

if [[ "$use_sudo" -eq 1 ]]; then
  kumabox_cmd=(sudo "$kumabox_path")
  remove_cmd=(sudo rm -rf)
  mkdir_cmd=(sudo mkdir -p)
  chmod_cmd=(sudo chmod)
  copy_cmd=(sudo cp)
  cat_cmd=(sudo cat)
else
  kumabox_cmd=("$kumabox_path")
  remove_cmd=(rm -rf)
  mkdir_cmd=(mkdir -p)
  chmod_cmd=(chmod)
  copy_cmd=(cp)
  cat_cmd=(cat)
fi

cni_conf_dir="$root_dir/cni/net.d"
cni_bin_dir="$root_dir/cni/bin"
cni_log="$root_dir/cni/plugin.log"
config_path="$root_dir/kumabox-multinic.toml"

cleanup() {
  set +e
  "${kumabox_cmd[@]}" --config "$config_path" delete "$name" --force >/dev/null 2>&1
  "${remove_cmd[@]}" "$root_dir" "$run_dir" "$log_dir"
}
trap cleanup EXIT

section "clean previous P2-09 state"
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
default = \"front\"
bridge = \"kumabox0\"
cidr = \"10.88.0.0/16\"
gateway = \"10.88.0.1\"
dns = [\"1.1.1.1\", \"8.8.8.8\"]
tap_prefix = \"kbtap\"
nat_backend = \"none\"
cni_config_dir = \"$cni_conf_dir\"
cni_bin_dir = \"$cni_bin_dir\""

for net in front back; do
  write_file "$cni_conf_dir/$net.conf" "{
  \"cniVersion\": \"1.0.0\",
  \"name\": \"$net\",
  \"type\": \"kumabox-mock\"
}"
done

write_file "$cni_bin_dir/kumabox-mock" "#!/bin/sh
set -eu
cat >/dev/null
printf '%s %s %s %s\n' \"\$CNI_COMMAND\" \"\$CNI_CONTAINERID\" \"\$CNI_IFNAME\" \"\$CNI_NETNS\" >> \"$cni_log\"
ns_name=\$(basename \"\$CNI_NETNS\")
case \"\$CNI_IFNAME\" in
  eth0) ip_addr=10.244.0.9 mac=5a:00:00:00:00:70 ;;
  eth1) ip_addr=10.244.1.9 mac=5a:00:00:00:00:71 ;;
  *) ip_addr=10.244.2.9 mac=5a:00:00:00:00:72 ;;
esac
if [ \"\$CNI_COMMAND\" = \"ADD\" ]; then
  ip netns exec \"\$ns_name\" ip link del \"\$CNI_IFNAME\" >/dev/null 2>&1 || true
  ip netns exec \"\$ns_name\" ip link add \"\$CNI_IFNAME\" type dummy
  ip netns exec \"\$ns_name\" ip link set dev \"\$CNI_IFNAME\" address \"\$mac\"
  ip netns exec \"\$ns_name\" ip addr add \"\$ip_addr/24\" dev \"\$CNI_IFNAME\"
  ip netns exec \"\$ns_name\" ip link set dev \"\$CNI_IFNAME\" up
  printf '{\"cniVersion\":\"1.0.0\",\"interfaces\":[{\"name\":\"%s\",\"mac\":\"%s\",\"sandbox\":\"%s\"}],\"ips\":[{\"address\":\"%s/24\",\"gateway\":\"10.244.0.1\",\"interface\":0}],\"dns\":{\"nameservers\":[\"1.1.1.1\"]}}\n' \"\$CNI_IFNAME\" \"\$mac\" \"\$CNI_NETNS\" \"\$ip_addr\"
  exit 0
fi
if [ \"\$CNI_COMMAND\" = \"DEL\" ]; then
  ip netns exec \"\$ns_name\" ip link del \"\$CNI_IFNAME\" >/dev/null 2>&1 || true
  exit 0
fi
exit 0"
"${chmod_cmd[@]}" +x "$cni_bin_dir/kumabox-mock"

section "create VM with two --network flags"
created_json="$("${kumabox_cmd[@]}" \
  --config "$config_path" \
  create \
  --name "$name" \
  --root-disk "$root_disk" \
  --firmware "$firmware" \
  --network cni:front \
  --network cni:back)"
printf '%s\n' "$created_json"

vm_id="$(printf '%s' "$created_json" | jq -r '.id')"
config_file="$(printf '%s' "$created_json" | jq -r '.config')"

if [[ "$(printf '%s' "$created_json" | jq -r '.network')" != "multi" ]]; then
  echo "legacy network summary should be multi" >&2
  exit 1
fi
if [[ "$(printf '%s' "$created_json" | jq -r '.networks | length')" != "2" ]]; then
  echo "VM networks[] should contain two attachments" >&2
  exit 1
fi
if [[ "$(printf '%s' "$created_json" | jq -r '.networkConfigs | length')" != "2" ]]; then
  echo "VM networkConfigs should contain two attachments" >&2
  exit 1
fi
printf 'state: vm=%s networks=%s\n' "$vm_id" "$(printf '%s' "$created_json" | jq -c '.networks')"
printf 'state: configs=%s\n' "$(printf '%s' "$created_json" | jq -c '[.networkConfigs[] | {networkName, ifName, tap, mac}]')"

section "network inspect"
network_json="$("${kumabox_cmd[@]}" --config "$config_path" network inspect "$name" --json)"
printf '%s\n' "$network_json"
if [[ "$(printf '%s' "$network_json" | jq -r '.interfaces | length')" != "2" ]]; then
  echo "provider index should contain two interfaces" >&2
  exit 1
fi
if [[ "$(printf '%s' "$network_json" | jq -r '.drift | length')" != "0" ]]; then
  echo "network inspect reported drift" >&2
  exit 1
fi

section "rendered Cloud Hypervisor net config"
"${cat_cmd[@]}" "$config_file" | jq '{netnsPath, nets}'
if [[ "$("${cat_cmd[@]}" "$config_file" | jq -r '.nets | length')" != "2" ]]; then
  echo "rendered Cloud Hypervisor config should contain two nets" >&2
  exit 1
fi

section "delete VM and verify cleanup"
"${kumabox_cmd[@]}" --config "$config_path" delete "$name" --force
if ! "${cat_cmd[@]}" "$cni_log" | grep -q "DEL $vm_id eth0 /var/run/netns/$vm_id"; then
  echo "missing CNI DEL for eth0" >&2
  "${cat_cmd[@]}" "$cni_log" || true
  exit 1
fi
if ! "${cat_cmd[@]}" "$cni_log" | grep -q "DEL $vm_id eth1 /var/run/netns/$vm_id"; then
  echo "missing CNI DEL for eth1" >&2
  "${cat_cmd[@]}" "$cni_log" || true
  exit 1
fi
if [[ "$("${kumabox_cmd[@]}" --config "$config_path" network ls --json | jq 'length')" != "0" ]]; then
  echo "provider records remain after delete" >&2
  exit 1
fi

echo "P2-09 multi-NIC network model verification passed"
