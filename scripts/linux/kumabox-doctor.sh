#!/usr/bin/env bash
set -Eeuo pipefail

# kumabox-doctor is the standalone host preflight/setup helper.  Keep it
# dependency-light so it can be fetched before KumaBox itself is installed.

cloud_hypervisor_version="${KUMABOX_CLOUD_HYPERVISOR_VERSION:-}"
cni_version="${KUMABOX_CNI_VERSION:-v1.5.1}"
firmware_version="${KUMABOX_FIRMWARE_VERSION:-}"
cloud_hypervisor_bin="${KUMABOX_CLOUD_HYPERVISOR_BIN:-/usr/local/bin/cloud-hypervisor}"
firmware_dir="${KUMABOX_FIRMWARE_DIR:-/usr/local/share/kumabox}"
cni_bin_dir="${KUMABOX_CNI_BIN_DIR:-/opt/cni/bin}"
cni_config_dir="${KUMABOX_CNI_CONFIG_DIR:-/etc/cni/net.d}"
json_output=0
upgrade=0
fix=0

usage() {
  cat <<'USAGE'
Usage: kumabox-doctor [--upgrade] [--fix] [--json]

Check whether this Linux host can run KumaBox.

  --upgrade  install missing host packages, Cloud Hypervisor, firmware, and CNI plugins
  --fix      create KumaBox/CNI directories and enable required kernel settings
  --json     print a machine-readable report

Environment overrides:
  KUMABOX_CLOUD_HYPERVISOR_VERSION, KUMABOX_FIRMWARE_VERSION, KUMABOX_CNI_VERSION
  KUMABOX_CLOUD_HYPERVISOR_BIN, KUMABOX_FIRMWARE_DIR, KUMABOX_CNI_BIN_DIR
  KUMABOX_CNI_CONFIG_DIR
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --upgrade) upgrade=1; shift ;;
    --fix) fix=1; shift ;;
    --json) json_output=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

checks=()
failures=0

add_check() {
  local name="$1" status="$2" code="$3" message="$4"
  checks+=("${name}|${status}|${code}|${message}")
  if [[ "$status" == fail ]]; then
    failures=$((failures + 1))
  fi
}

command_exists() { command -v "$1" >/dev/null 2>&1; }

check_command() {
  local name="$1" command_name="$2" code="$3"
  if command_exists "$command_name"; then
    add_check "$name" pass "" "$(command -v "$command_name") is available"
  else
    add_check "$name" fail "$code" "$command_name is missing"
  fi
}

check_file() {
  local name="$1" path="$2" code="$3"
  if [[ -e "$path" ]]; then
    add_check "$name" pass "" "$path exists"
  else
    add_check "$name" fail "$code" "$path is missing"
  fi
}

check_executable() {
  local name="$1" path="$2" code="$3"
  if [[ -x "$path" ]]; then
    add_check "$name" pass "" "$path is executable"
  else
    add_check "$name" fail "$code" "$path is missing or not executable"
  fi
}

if [[ "$(uname -s)" != Linux ]]; then
  add_check host fail NOT_LINUX "KumaBox requires a Linux host"
else
  add_check host pass "" "running on Linux"
  arch="$(uname -m)"
  case "$arch" in
    x86_64|aarch64) add_check architecture pass "" "supported architecture: $arch" ;;
    *) add_check architecture fail UNSUPPORTED_ARCH "unsupported architecture: $arch" ;;
  esac

  check_file kvm /dev/kvm KVM_DEVICE_MISSING
  if [[ -r /dev/kvm && -w /dev/kvm ]]; then
    add_check kvmPermission pass "" "/dev/kvm is readable and writable"
  else
    add_check kvmPermission fail KVM_PERMISSION_DENIED "current user cannot read/write /dev/kvm"
  fi

  if [[ "$arch" == x86_64 ]] && grep -Eq '(^| )(vmx|svm)( |$)' /proc/cpuinfo 2>/dev/null; then
    add_check cpuVirtualization pass "" "hardware virtualization flag is present"
  elif [[ "$arch" == aarch64 ]]; then
    add_check cpuVirtualization pass "" "hardware virtualization is provided by the ARM KVM host"
  else
    add_check cpuVirtualization fail CPU_VIRT_FLAG_MISSING "vmx/svm CPU flag is missing"
  fi
fi

check_command cloudHypervisor "$cloud_hypervisor_bin" CH_MISSING
check_command qemuImg qemu-img QEMU_IMG_MISSING
check_file networkTun /dev/net/tun TUNTAP_MISSING
check_command ip ip IPROUTE2_MISSING
if command_exists iptables || command_exists nft; then
  add_check networkNAT pass "" "iptables or nft is available"
else
  add_check networkNAT fail NAT_BACKEND_MISSING "neither iptables nor nft is available"
fi

for plugin in bridge host-local loopback; do
  check_executable "cni:${plugin}" "$cni_bin_dir/$plugin" CNI_PLUGIN_MISSING
done
if [[ -d "$cni_config_dir" ]]; then
  add_check cniConfig pass "" "$cni_config_dir exists"
else
  add_check cniConfig fail CNI_CONFIG_MISSING "$cni_config_dir is missing"
fi

firmware_path="$firmware_dir/CLOUDHV.fd"
check_file firmware "$firmware_path" FIRMWARE_MISSING

if [[ "$fix" -eq 1 ]]; then
  if [[ "$(id -u)" -ne 0 ]]; then
    add_check fix fail ROOT_REQUIRED "--fix must be run as root or through sudo"
  else
    mkdir -p "$firmware_dir" "$cni_bin_dir" "$cni_config_dir"
    if command_exists sysctl; then
      sysctl -w net.ipv4.ip_forward=1 >/dev/null
      sysctl -w net.ipv4.conf.all.rp_filter=0 >/dev/null
    fi
    add_check fix pass "" "KumaBox and networking directories are ready"
  fi
fi

latest_asset() {
  local repo="$1" pattern="$2" version="${3:-latest}" api
  if [[ "$version" == latest ]]; then
    api="https://api.github.com/repos/$repo/releases/latest"
  else
    api="https://api.github.com/repos/$repo/releases/tags/$version"
  fi
  curl -fsSL "$api" |
    grep -o '"browser_download_url": "[^"]*"' |
    sed -E 's/^"browser_download_url": "(.*)"$/\1/' |
    grep -E "$pattern" | head -n 1
}

download_to() {
  local url="$1" destination="$2"
  local temp
  temp="$(mktemp)"
  trap 'rm -f "$temp"' RETURN
  curl -fsSL --retry 3 -o "$temp" "$url"
  if tar -tzf "$temp" >/dev/null 2>&1; then
    local extracted candidate
    extracted="$(mktemp -d)"
    tar -xzf "$temp" -C "$extracted"
    candidate="$(find "$extracted" -type f -name 'cloud-hypervisor*' -perm -u+x | head -n 1)"
    [[ -n "$candidate" ]] || { echo "archive does not contain a Cloud Hypervisor binary: $url" >&2; return 1; }
    install -m 0755 "$candidate" "$destination"
    rm -rf "$extracted"
  else
    install -m 0755 "$temp" "$destination"
  fi
}

upgrade_host_packages() {
  if [[ "$(id -u)" -ne 0 ]]; then
    echo "--upgrade must be run as root or through sudo" >&2
    return 1
  fi
  if command_exists apt-get; then
    apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl iproute2 iptables qemu-utils
  elif command_exists dnf; then
    dnf install -y ca-certificates curl iproute iptables qemu-img
  elif command_exists apk; then
    apk add --no-cache ca-certificates curl iproute2 iptables qemu-img
  else
    echo "cannot install packages automatically: supported package managers are apt-get, dnf, and apk" >&2
  fi
}

if [[ "$upgrade" -eq 1 ]]; then
  upgrade_host_packages
  mkdir -p "$firmware_dir" "$cni_bin_dir" "$cni_config_dir"
  if [[ -z "$cloud_hypervisor_version" ]]; then cloud_hypervisor_version=latest; fi
  if [[ -z "$firmware_version" ]]; then firmware_version=latest; fi
  arch="$(uname -m)"
  ch_asset="$(latest_asset cloud-hypervisor/cloud-hypervisor 'cloud-hypervisor.*(x86_64|aarch64)' "$cloud_hypervisor_version")"
  fw_asset="$(latest_asset cloud-hypervisor/rust-hypervisor-firmware 'hypervisor-fw' "$firmware_version")"
  cni_asset="https://github.com/containernetworking/plugins/releases/download/$cni_version/cni-plugins-linux-${arch}-${cni_version}.tgz"
  [[ -n "$ch_asset" ]] || { echo "could not find a Cloud Hypervisor release asset" >&2; exit 1; }
  [[ -n "$fw_asset" ]] || { echo "could not find a firmware release asset" >&2; exit 1; }
  download_to "$ch_asset" "$cloud_hypervisor_bin"
  download_to "$fw_asset" "$firmware_path"
  cni_tmp="$(mktemp -d)"
  trap 'rm -rf "$cni_tmp"' EXIT
  curl -fsSL --retry 3 "$cni_asset" | tar -xzf - -C "$cni_tmp"
  install -m 0755 "$cni_tmp"/{bridge,host-local,loopback} "$cni_bin_dir/"
  echo "KumaBox host dependencies installed. Run kumabox-doctor to verify them."
  exit 0
fi

if [[ "$json_output" -eq 1 ]]; then
  printf '{"status":"%s","checks":[' "$([[ "$failures" -eq 0 ]] && echo pass || echo fail)"
  first=1
  for check in "${checks[@]}"; do
    IFS='|' read -r name status code message <<< "$check"
    [[ "$first" -eq 0 ]] && printf ','
    first=0
    printf '{"name":"%s","status":"%s","code":"%s","message":"%s"}' "$name" "$status" "$code" "$message"
  done
  printf ']}'
  printf '\n'
else
  for check in "${checks[@]}"; do
    IFS='|' read -r name status code message <<< "$check"
    if [[ -n "$code" ]]; then printf '%s: %s (%s): %s\n' "$status" "$name" "$code" "$message"; else printf '%s: %s: %s\n' "$status" "$name" "$message"; fi
  done
fi

exit "$([[ "$failures" -eq 0 ]] && echo 0 || echo 1)"
