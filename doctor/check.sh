#!/usr/bin/env bash
set -Eeuo pipefail

# Standalone host setup and verification for released KumaBox binaries.
root_dir=${KUMABOX_ROOT_DIR:-/var/lib/kumabox}
run_dir=${KUMABOX_RUN_DIR:-/var/lib/kumabox/run}
log_dir=${KUMABOX_LOG_DIR:-/var/log/kumabox}
cni_bin_dir=${KUMABOX_CNI_BIN_DIR:-/opt/cni/bin}
cni_config_dir=${KUMABOX_CNI_CONFIG_DIR:-/etc/cni/net.d}
cloud_hypervisor_version=${KUMABOX_CLOUD_HYPERVISOR_VERSION:-v51.1}
firmware_version=${KUMABOX_FIRMWARE_VERSION:-0.5.0}
cni_version=${KUMABOX_CNI_VERSION:-v1.9.0}
erofs_version=${KUMABOX_EROFS_VERSION:-v1.8.10}
network_name=${KUMABOX_NETWORK_NAME:-kumabox}
subnet=${KUMABOX_NETWORK_SUBNET:-10.88.0.0/16}
upgrade=false
fix=false

usage() {
	cat <<EOF
Usage: kumabox-check [--fix] [--upgrade]

Check a Linux host for KumaBox. --fix creates runtime/network configuration;
--upgrade also installs host packages, Cloud Hypervisor, firmware, EROFS tools,
and CNI plugins. Both mutation modes require root.

Pinned dependency defaults:
  Cloud Hypervisor ${cloud_hypervisor_version}
  hypervisor-fw    ${firmware_version}
  CNI plugins      ${cni_version}
  erofs-utils      ${erofs_version}

Environment overrides use the KUMABOX_* variables documented in README.md.
EOF
}

while (($#)); do
	case "$1" in
		--fix) fix=true; shift ;;
		--upgrade) fix=true; upgrade=true; shift ;;
		-h|--help) usage; exit 0 ;;
		*) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
	esac
done

pass_count=0
warn_count=0
fail_count=0
pass() { pass_count=$((pass_count + 1)); printf '  [PASS] %s\n' "$1"; }
warn() { warn_count=$((warn_count + 1)); printf '  [WARN] %s\n' "$1"; }
fail() { fail_count=$((fail_count + 1)); printf '  [FAIL] %s\n' "$1"; }
fixed() { printf '  [FIXED] %s\n' "$1"; }
info() { printf '  [INFO] %s\n' "$1"; }
section() { printf '\n==> %s\n' "$1"; }
exists() { command -v "$1" >/dev/null 2>&1; }

require_root() {
	if [[ $(id -u) -ne 0 ]]; then
		echo "$1 requires root; run through sudo" >&2
		exit 1
	fi
}

arch=$(uname -m)
case "$arch" in
	x86_64) go_arch=amd64; ch_suffix=; firmware_suffix= ;;
	aarch64|arm64) go_arch=arm64; ch_suffix=-aarch64; firmware_suffix=-aarch64 ;;
	*) go_arch=; ch_suffix=; firmware_suffix= ;;
esac

install_packages() {
	section "Host packages"
	if exists apt-get; then
		apt-get update
		DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
			build-essential autoconf automake ca-certificates curl git iproute2 \
			iptables jq liblz4-dev liblzma-dev libtool libuuid1 libuuid-dev \
			libzstd-dev nftables pkg-config qemu-utils tar xz-utils zlib1g-dev
	elif exists dnf; then
		dnf install -y autoconf automake ca-certificates curl gcc git iproute \
			iptables jq libtool libuuid-devel libzstd-devel lz4-devel make \
			nftables pkgconf-pkg-config qemu-img tar xz-devel zlib-devel
	else
		echo "automatic installation currently supports apt-get and dnf" >&2
		exit 1
	fi
	fixed "host packages installed"
}

download_binary() {
	local url=$1 destination=$2 temporary
	temporary=$(mktemp)
	curl -fsSL --retry 3 -o "$temporary" "$url"
	install -m 0755 "$temporary" "$destination"
	rm -f "$temporary"
}

erofs_version_ok() {
	local version
	version=$(mkfs.erofs --version 2>&1 | sed -n 's/.*[Vv]\?\([0-9][0-9]*\.[0-9][0-9]*\).*/\1/p' | head -n 1)
	[[ -n "$version" ]] || return 1
	local major=${version%%.*} minor=${version#*.}
	((major > 1 || (major == 1 && minor >= 8)))
}

install_erofs() {
	if exists mkfs.erofs && erofs_version_ok; then
		fixed "mkfs.erofs is already 1.8 or newer"
		return
	fi
	section "erofs-utils ${erofs_version}"
	local temporary source
	temporary=$(mktemp -d)
	curl -fsSL --retry 3 -o "$temporary/erofs.tar.gz" \
		"https://github.com/erofs/erofs-utils/archive/refs/tags/${erofs_version}.tar.gz"
	tar -xzf "$temporary/erofs.tar.gz" -C "$temporary"
	source=$(find "$temporary" -mindepth 1 -maxdepth 1 -type d -name 'erofs-utils-*' | head -n 1)
	[[ -n "$source" ]] || { echo "erofs-utils source archive is invalid" >&2; exit 1; }
	(
		cd "$source"
		./autogen.sh
		./configure --prefix=/usr/local
		make -j"$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 2)"
		make install
	)
	erofs_version_ok || { echo "installed mkfs.erofs is older than 1.8" >&2; exit 1; }
	rm -rf "$temporary"
	fixed "erofs-utils ${erofs_version} installed"
}

install_dependencies() {
	require_root "--upgrade"
	[[ $(uname -s) == Linux && -n "$go_arch" ]] || { echo "unsupported host: $(uname -s)/$arch" >&2; exit 1; }
	install_packages
	section "Cloud Hypervisor ${cloud_hypervisor_version}"
	download_binary \
		"https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${cloud_hypervisor_version}/cloud-hypervisor-static${ch_suffix}" \
		/usr/local/bin/cloud-hypervisor
	fixed "cloud-hypervisor installed"
	section "hypervisor firmware ${firmware_version}"
	install -d -m 0755 "$root_dir/firmware"
	download_binary \
		"https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/${firmware_version}/hypervisor-fw${firmware_suffix}" \
		"$root_dir/firmware/CLOUDHV.fd"
	fixed "firmware installed"
	section "CNI plugins ${cni_version}"
	local temporary
	temporary=$(mktemp -d)
	curl -fsSL --retry 3 -o "$temporary/cni.tgz" \
		"https://github.com/containernetworking/plugins/releases/download/${cni_version}/cni-plugins-linux-${go_arch}-${cni_version}.tgz"
	install -d -m 0755 "$cni_bin_dir"
	tar -xzf "$temporary/cni.tgz" -C "$temporary"
	for plugin in bridge host-local loopback; do
		install -m 0755 "$temporary/$plugin" "$cni_bin_dir/$plugin"
	done
	rm -rf "$temporary"
	fixed "CNI plugins installed"
	install_erofs
}

gateway_for_subnet() {
	local address=${subnet%/*}
	local prefix=${subnet#*/}
	local first second third fourth
	IFS=. read -r first second third fourth <<< "$address"
	[[ $address != "$subnet" && $prefix =~ ^[0-9]+$ && $prefix -ge 16 && $prefix -le 24 ]] || {
		echo "KUMABOX_NETWORK_SUBNET must be an IPv4 /16 through /24 CIDR" >&2
		exit 1
	}
	for octet in "$first" "$second" "$third" "$fourth"; do
		[[ $octet =~ ^[0-9]+$ && $octet -le 255 ]] || {
			echo "invalid IPv4 subnet: $subnet" >&2
			exit 1
		}
	done
	printf '%s.%s.%s.1' "$first" "$second" "$third"
}

configure_host() {
	require_root "--fix"
	install -d -m 0750 "$root_dir" "$run_dir" "$root_dir/metadata"
	install -d -m 0755 "$log_dir" "$cni_config_dir" /var/run/netns
	cat > /etc/sysctl.d/99-kumabox.conf <<'EOF'
net.ipv4.ip_forward = 1
net.bridge.bridge-nf-call-iptables = 1
EOF
	modprobe br_netfilter 2>/dev/null || true
	sysctl --system >/dev/null
	local gateway host_iface host_mtu config_path
	gateway=$(gateway_for_subnet)
	host_iface=$(ip route show default | awk '/default/{print $5; exit}')
	host_mtu=$(ip -o link show "$host_iface" 2>/dev/null | sed -n 's/.* mtu \([0-9][0-9]*\).*/\1/p')
	host_mtu=${host_mtu:-1500}
	config_path="$cni_config_dir/10-kumabox.conflist"
	if [[ ! -e "$config_path" ]]; then
		cat > "$config_path" <<EOF
{
  "cniVersion": "1.0.0",
  "name": "${network_name}",
  "plugins": [{
    "type": "bridge",
    "bridge": "kbcni0",
    "mtu": ${host_mtu},
    "isGateway": true,
    "ipMasq": true,
    "hairpinMode": true,
    "ipam": {
      "type": "host-local",
      "ranges": [[{"subnet": "${subnet}", "gateway": "${gateway}"}]],
      "routes": [{"dst": "0.0.0.0/0"}]
    }
  }]
}
EOF
		fixed "created $config_path"
	else
		info "preserving existing $config_path"
	fi
	if exists iptables; then
		iptables -C FORWARD -i kbcni0 -j ACCEPT 2>/dev/null || iptables -A FORWARD -i kbcni0 -j ACCEPT
		iptables -C FORWARD -o kbcni0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || \
			iptables -A FORWARD -o kbcni0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
	fi
	fixed "KumaBox directories, sysctl, and CNI configuration are ready"
}

if $upgrade; then install_dependencies; fi
if $fix; then configure_host; fi

section "Host"
if [[ $(uname -s) == Linux ]]; then pass "Linux host"; else fail "KumaBox requires Linux"; fi
if [[ -n "$go_arch" ]]; then pass "supported architecture: $arch"; else fail "unsupported architecture: $arch"; fi
if [[ -r /dev/kvm && -w /dev/kvm ]]; then pass "/dev/kvm is accessible"; else fail "/dev/kvm is missing or inaccessible"; fi
if [[ -e /dev/net/tun ]]; then pass "/dev/net/tun exists"; else fail "/dev/net/tun is missing"; fi

section "Binaries"
for binary in cloud-hypervisor qemu-img ip jq; do
	if exists "$binary"; then pass "$binary: $(command -v "$binary")"; else fail "$binary is missing"; fi
done
if exists mkfs.erofs && erofs_version_ok; then pass "mkfs.erofs 1.8+"; else fail "mkfs.erofs 1.8+ is required"; fi

section "CNI"
for plugin in bridge host-local loopback; do
	if [[ -x "$cni_bin_dir/$plugin" ]]; then pass "$plugin plugin"; else fail "$plugin plugin is missing"; fi
done
if [[ -f "$cni_config_dir/10-kumabox.conflist" ]]; then pass "cni:${network_name} configuration"; else fail "KumaBox CNI conflist is missing"; fi
if [[ $(sysctl -n net.ipv4.ip_forward 2>/dev/null || true) == 1 ]]; then pass "IPv4 forwarding"; else fail "IPv4 forwarding is disabled"; fi

section "Directories"
for directory in "$root_dir" "$run_dir" "$log_dir"; do
	if [[ -d "$directory" ]]; then pass "$directory"; else fail "$directory is missing"; fi
done

printf '\nSummary: pass=%d warn=%d fail=%d\n' "$pass_count" "$warn_count" "$fail_count"
if ((fail_count > 0)); then
	$fix || info "run: sudo kumabox-check --upgrade"
	exit 1
fi
