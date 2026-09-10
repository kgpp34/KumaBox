#!/usr/bin/env bash
# doctor/check.sh — Pre-flight check and repair tool for KumaBox.
#
# Usage:
#   ./doctor/check.sh              # Check only
#   ./doctor/check.sh --fix        # Check and fix issues
#   ./doctor/check.sh --upgrade    # Check, fix, and upgrade dependencies

set -uo pipefail

# ---------------------------------------------------------------------------
# Configuration (override via environment)
# ---------------------------------------------------------------------------
KUMABOX_ROOT_DIR="${KUMABOX_ROOT_DIR:-/var/lib/kumabox}"
KUMABOX_RUN_DIR="${KUMABOX_RUN_DIR:-/run/kumabox}"
KUMABOX_LOG_DIR="${KUMABOX_LOG_DIR:-/var/log/kumabox}"
KUMABOX_CNI_CONF_DIR="${KUMABOX_CNI_CONF_DIR:-/etc/cni/net.d}"
KUMABOX_CNI_BIN_DIR="${KUMABOX_CNI_BIN_DIR:-/opt/cni/bin}"

# Dependency versions
CH_VERSION="${CH_VERSION:-v53.0}"
CNI_VERSION="${CNI_VERSION:-v1.9.1}"

# Architecture detection
ARCH=$(uname -m)
case "$ARCH" in
    x86_64)  GO_ARCH="amd64"; CH_SUFFIX="" ;;
    aarch64) GO_ARCH="arm64"; CH_SUFFIX="-aarch64" ;;
    *) echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

# ---------------------------------------------------------------------------
# Flags
# ---------------------------------------------------------------------------
FIX=false
UPGRADE=false
SUBNET=""
for arg in "$@"; do
    case "$arg" in
        --fix)     FIX=true ;;
        --upgrade) FIX=true; UPGRADE=true ;;
        --subnet=*) SUBNET="${arg#--subnet=}" ;;
        -h|--help)
            cat <<EOF
Usage: $0 [--fix] [--upgrade] [--subnet=CIDR]

Options:
  --fix            Attempt to fix detected issues (dirs, sysctl, iptables, CNI config)
  --upgrade        Fix issues and install/upgrade dependencies:
                     cloud-hypervisor ${CH_VERSION}
                     CNI plugins      ${CNI_VERSION}
  --subnet=CIDR    Subnet for generated CNI bridge config (default: 10.88.0.0/16)

Environment variables:
  CH_VERSION    Cloud Hypervisor version    (default: ${CH_VERSION})
  CNI_VERSION   CNI plugins version         (default: ${CNI_VERSION})
  KUMABOX_ROOT_DIR / KUMABOX_RUN_DIR / KUMABOX_LOG_DIR
  KUMABOX_CNI_CONF_DIR / KUMABOX_CNI_BIN_DIR
EOF
            exit 0
            ;;
    esac
done

# ---------------------------------------------------------------------------
# Output helpers
# ---------------------------------------------------------------------------
PASS=0; WARN=0; FAIL=0

pass()   { PASS=$((PASS + 1)); printf "  \033[38;5;42m[PASS]\033[0m %s\n" "$1"; }
warn()   { WARN=$((WARN + 1)); printf "  \033[38;5;214m[WARN]\033[0m %s\n" "$1"; }
fail()   { FAIL=$((FAIL + 1)); printf "  \033[38;5;203m[FAIL]\033[0m %s\n" "$1"; }
info()   { printf "  \033[38;5;75m[INFO]\033[0m %s\n" "$1"; }
fixed()  { printf "  \033[38;5;81m[FIXED]\033[0m %s\n" "$1"; }
header() { printf "\n\033[1;38;5;111m%s\033[0m\n" "$1"; }

# ---------------------------------------------------------------------------
# CNI conflist generator
# ---------------------------------------------------------------------------
generate_cni_conflist() {
    local subnet="${SUBNET:-10.88.0.0/16}"

    # Extract gateway: replace last octet of network address with .1
    local network_part
    network_part=$(echo "$subnet" | cut -d/ -f1)
    local prefix_len
    prefix_len=$(echo "$subnet" | cut -d/ -f2)
    # Simple gateway: network address with last octet = 1
    local gateway
    gateway=$(echo "$network_part" | awk -F. '{printf "%s.%s.%s.1", $1, $2, $3}')

    # Match the host egress MTU (GCP is 1460, not 1500) so the bridge and TAPs do not
    # blackhole large packets on the way out.
    local host_iface host_mtu
    host_iface=$(ip route show default 2>/dev/null | awk '/default/{print $5; exit}')
    host_mtu=$(ip link show "$host_iface" 2>/dev/null | sed -n 's/.* mtu \([0-9]\{1,\}\).*/\1/p')
    host_mtu=${host_mtu:-1500}

    info "generating CNI conflist: subnet=${subnet} gateway=${gateway} mtu=${host_mtu}"
    mkdir -p "$KUMABOX_CNI_CONF_DIR"
    cat > "$CNI_CONFLIST" <<CNIEOF
{
  "cniVersion": "1.0.0",
  "name": "kumabox",
  "plugins": [
    {
      "type": "bridge",
      "bridge": "cni0",
      "mtu": ${host_mtu},
      "isGateway": true,
      "ipMasq": true,
      "hairpinMode": true,
      "ipam": {
        "type": "host-local",
        "ranges": [
          [{"subnet": "${subnet}", "gateway": "${gateway}"}]
        ],
        "routes": [
          {"dst": "0.0.0.0/0"}
        ]
      }
    }
  ]
}
CNIEOF
    fixed "generated $CNI_CONFLIST (subnet: ${subnet})"
}

# ---------------------------------------------------------------------------
# 1. Binary dependencies
# ---------------------------------------------------------------------------
header "Binary dependencies"

# Map binary name → apt package name for auto-install.
bin_to_pkg() {
    case "$1" in
        mkfs.erofs) echo "erofs-utils" ;;
        mkfs.ext4)  echo "e2fsprogs" ;;
        *)          echo "" ;;
    esac
}

# Mirror the runtime floor in images/oci/erofs.go: erofs-utils < 1.8 tar mode
# silently corrupts layers, so doctor must not PASS a host kumabox will refuse.
erofs_version_ok() {
    local xy major minor
    xy=$(echo "$1" | grep -oE '[0-9]+\.[0-9]+' | head -1)
    [ -n "$xy" ] || return 1
    major=${xy%%.*}; minor=${xy#*.}
    [ "$major" -gt 1 ] || { [ "$major" -eq 1 ] && [ "$minor" -ge 8 ]; }
}

check_binary() {
    local name="$1"
    if command -v "$name" &>/dev/null; then
        local ver=""
        case "$name" in
            cloud-hypervisor) ver=$("$name" --version 2>/dev/null | head -1) || true ;;
            ch-remote)        ver=$("$name" --version 2>/dev/null | head -1) || true ;;
            mkfs.ext4)        ver=$("$name" -V 2>&1 | head -1) || true ;;
            mkfs.erofs)       ver=$("$name" --version 2>&1 | head -1) || true ;;
        esac
        if [ "$name" = "mkfs.erofs" ] && ! erofs_version_ok "$ver"; then
            fail "$name (${ver:-unknown}) is older than 1.8 — tar mode silently corrupts layers; apt ships 1.7.x, install erofs-utils >= 1.8 from source"
            return
        fi
        pass "${name}${ver:+ ($ver)}"
    else
        fail "$name not found in PATH"
        if $FIX; then
            local pkg
            pkg=$(bin_to_pkg "$name")
            if [ -n "$pkg" ] && command -v apt-get &>/dev/null; then
                apt-get install -y "$pkg" &>/dev/null && fixed "apt-get install $pkg" || warn "failed to install $pkg"
                if [ "$name" = "mkfs.erofs" ] && command -v mkfs.erofs &>/dev/null \
                    && ! erofs_version_ok "$(mkfs.erofs --version 2>&1 | head -1)"; then
                    warn "installed mkfs.erofs is still older than 1.8 — install erofs-utils from source"
                fi
            fi
        fi
    fi
}

check_binary cloud-hypervisor
check_binary ch-remote
check_binary mkfs.ext4
check_binary mkfs.erofs

# ---------------------------------------------------------------------------
# 2. KVM access
# ---------------------------------------------------------------------------
header "KVM"

if [ -e /dev/kvm ]; then
    if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then
        pass "/dev/kvm accessible"
    else
        fail "/dev/kvm exists but not readable/writable by $(whoami)"
        if $FIX; then
            chmod 666 /dev/kvm 2>/dev/null && fixed "chmod 666 /dev/kvm" || warn "failed to fix (need root?)"
        fi
    fi
else
    fail "/dev/kvm not found (nested virtualization or bare-metal required)"
fi

# ---------------------------------------------------------------------------
# 3. Managed directories
# ---------------------------------------------------------------------------
header "Directories"

check_dir() {
    local dir="$1"
    if [ -d "$dir" ]; then
        pass "$dir"
    else
        fail "$dir does not exist"
        if $FIX; then
            mkdir -p "$dir" && fixed "created $dir" || warn "failed to create $dir"
        fi
    fi
}

check_dir "$KUMABOX_ROOT_DIR"

# SQLite WAL needs coherent shared memory. KumaBox has one metadata engine and
# does not probe or preserve Cocoon's per-backend JSON stores.
meta_fstype=$(stat -f -c %T "$KUMABOX_ROOT_DIR" 2>/dev/null || echo unknown)
case "$meta_fstype" in
    nfs*|cifs|smb*|fuse*)
        fail "meta root on $meta_fstype: sqlite WAL needs coherent shared memory; kumabox refuses this filesystem"
        ;;
    *)
        pass "meta root filesystem ($meta_fstype) supports WAL"
        ;;
esac

check_dir "$KUMABOX_RUN_DIR"
check_dir "$KUMABOX_LOG_DIR"
check_dir "${KUMABOX_ROOT_DIR}/meta"
check_dir "${KUMABOX_ROOT_DIR}/images/blobs"
check_dir "${KUMABOX_ROOT_DIR}/images/layers"
check_dir "${KUMABOX_ROOT_DIR}/images/boot"
check_dir "${KUMABOX_ROOT_DIR}/sandboxes"
check_dir "${KUMABOX_ROOT_DIR}/snapshots"
check_dir "${KUMABOX_ROOT_DIR}/network/cni-cache"
check_dir "${KUMABOX_ROOT_DIR}/staging/imports"
check_dir "${KUMABOX_ROOT_DIR}/staging/snapshots"
check_dir "${KUMABOX_ROOT_DIR}/staging/restores"
check_dir "${KUMABOX_RUN_DIR}/locks/sandboxes"
check_dir "${KUMABOX_RUN_DIR}/sandboxes"
check_dir "${KUMABOX_LOG_DIR}/sandboxes"
check_dir /run/netns

# ---------------------------------------------------------------------------
# 4. Sysctl
# ---------------------------------------------------------------------------
header "Sysctl"

check_sysctl() {
    local key="$1"
    local expected="$2"
    local actual
    actual=$(sysctl -n "$key" 2>/dev/null || echo "")
    if [ "$actual" = "$expected" ]; then
        pass "$key = $expected"
    else
        fail "$key = ${actual:-<unset>} (expected $expected)"
        if $FIX; then
            sysctl -w "${key}=${expected}" &>/dev/null && fixed "sysctl -w ${key}=${expected}" || warn "failed to set $key"
        fi
    fi
}

check_sysctl net.ipv4.ip_forward 1

# br_netfilter must be loaded for bridge sysctl keys to exist.
if ! sysctl -n net.bridge.bridge-nf-call-iptables &>/dev/null; then
    if $FIX; then
        modprobe br_netfilter 2>/dev/null && fixed "modprobe br_netfilter" || warn "failed to load br_netfilter"
    fi
fi
check_sysctl net.bridge.bridge-nf-call-iptables 1

# ---------------------------------------------------------------------------
# 5. iptables FORWARD rules for CNI bridge
# ---------------------------------------------------------------------------
header "iptables FORWARD (cni0)"

check_iptables_rule() {
    local desc="$1"
    shift
    if iptables -C "$@" 2>/dev/null; then
        pass "$desc"
    else
        fail "$desc"
        if $FIX; then
            iptables -A "$@" 2>/dev/null && fixed "iptables -A $*" || warn "failed to add rule"
        fi
    fi
}

check_iptables_rule "FORWARD -i cni0 -j ACCEPT" \
    FORWARD -i cni0 -j ACCEPT
check_iptables_rule "FORWARD -o cni0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT" \
    FORWARD -o cni0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT

# Clamp TCP MSS to the path MTU: on a host whose egress MTU is below the bridge's
# (e.g. GCP's 1460), guests otherwise blackhole large TLS/data packets that carry DF.
mss_desc="mangle FORWARD TCPMSS clamp-mss-to-pmtu"
if iptables -t mangle -C FORWARD -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu 2>/dev/null; then
    pass "$mss_desc"
else
    fail "$mss_desc"
    if $FIX; then
        iptables -t mangle -A FORWARD -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu 2>/dev/null \
            && fixed "$mss_desc" || warn "failed to add MSS clamp"
    fi
fi

# ---------------------------------------------------------------------------
# 6. CNI configuration
# ---------------------------------------------------------------------------
header "CNI configuration"

CNI_CONFLIST="${KUMABOX_CNI_CONF_DIR}/10-kumabox.conflist"

if [ -d "$KUMABOX_CNI_CONF_DIR" ]; then
    conflist_count=$(find "$KUMABOX_CNI_CONF_DIR" -maxdepth 1 -name '*.conflist' 2>/dev/null | wc -l)
    if [ "$conflist_count" -gt 0 ]; then
        first=$(find "$KUMABOX_CNI_CONF_DIR" -maxdepth 1 -name '*.conflist' 2>/dev/null | sort | head -1)
        pass "conflist: $(basename "$first")"
    else
        fail "no .conflist files in $KUMABOX_CNI_CONF_DIR"
        if $FIX; then
            generate_cni_conflist
        fi
    fi
else
    fail "$KUMABOX_CNI_CONF_DIR does not exist"
    if $FIX; then
        mkdir -p "$KUMABOX_CNI_CONF_DIR" && fixed "created $KUMABOX_CNI_CONF_DIR" || warn "failed"
        generate_cni_conflist
    fi
fi

# ---------------------------------------------------------------------------
# 7. CNI plugins
# ---------------------------------------------------------------------------
header "CNI plugins (${KUMABOX_CNI_BIN_DIR})"

CNI_REQUIRED="bridge host-local loopback"

if [ -d "$KUMABOX_CNI_BIN_DIR" ]; then
    for plugin in $CNI_REQUIRED; do
        if [ -x "${KUMABOX_CNI_BIN_DIR}/${plugin}" ]; then
            pass "$plugin"
        else
            fail "$plugin not found"
        fi
    done
else
    fail "$KUMABOX_CNI_BIN_DIR does not exist"
fi

# ---------------------------------------------------------------------------
# 8. Upgrade / Install
# ---------------------------------------------------------------------------
if $UPGRADE; then
    tmpdir=$(mktemp -d)
    trap 'rm -rf "$tmpdir"' EXIT

    # -- cloud-hypervisor --------------------------------------------------
    header "Install cloud-hypervisor ${CH_VERSION}"

    ch_url="https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/cloud-hypervisor-static${CH_SUFFIX}"
    ch_dest="/usr/local/bin/cloud-hypervisor"
    info "downloading ${ch_url}"
    if curl -fsSL -o "${tmpdir}/cloud-hypervisor" "$ch_url"; then
        install -m 0755 "${tmpdir}/cloud-hypervisor" "$ch_dest"
        # virtio-net requires CAP_NET_ADMIN for tap devices
        setcap cap_net_admin+ep "$ch_dest" 2>/dev/null || true
        fixed "cloud-hypervisor ${CH_VERSION} -> ${ch_dest}"
    else
        fail "failed to download cloud-hypervisor from ${ch_url}"
    fi

    # -- ch-remote ----------------------------------------------------------
    header "Install ch-remote ${CH_VERSION}"

    chr_url="https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/ch-remote-static${CH_SUFFIX}"
    chr_dest="/usr/local/bin/ch-remote"
    info "downloading ${chr_url}"
    if curl -fsSL -o "${tmpdir}/ch-remote" "$chr_url"; then
        install -m 0755 "${tmpdir}/ch-remote" "$chr_dest"
        fixed "ch-remote ${CH_VERSION} -> ${chr_dest}"
    else
        fail "failed to download ch-remote from ${chr_url}"
    fi

    # -- CNI plugins --------------------------------------------------------
    header "Install CNI plugins ${CNI_VERSION}"

    cni_tarball="cni-plugins-linux-${GO_ARCH}-${CNI_VERSION}.tgz"
    cni_url="https://github.com/containernetworking/plugins/releases/download/${CNI_VERSION}/${cni_tarball}"
    info "downloading ${cni_url}"
    if curl -fsSL -o "${tmpdir}/${cni_tarball}" "$cni_url"; then
        mkdir -p "$KUMABOX_CNI_BIN_DIR"
        tar -xzf "${tmpdir}/${cni_tarball}" -C "$KUMABOX_CNI_BIN_DIR"
        fixed "CNI plugins ${CNI_VERSION} -> ${KUMABOX_CNI_BIN_DIR}"
        info "installed plugins:"
        for p in "$KUMABOX_CNI_BIN_DIR"/*; do
            [ -x "$p" ] && info "  $(basename "$p")"
        done
    else
        fail "failed to download CNI plugins from ${cni_url}"
    fi
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
header "Summary"
printf "  \033[38;5;42m%d passed\033[0m · \033[38;5;214m%d warnings\033[0m · \033[38;5;203m%d failed\033[0m\n\n" \
    "$PASS" "$WARN" "$FAIL"

if [ "$FAIL" -gt 0 ] && ! $FIX; then
    info "Run '$0 --fix' to attempt automatic fixes"
    info "Run '$0 --upgrade' to install/upgrade cloud-hypervisor and CNI plugins"
fi

[ "$FAIL" -eq 0 ] || exit 1
