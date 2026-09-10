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
KUMABOX_META_DIR="${KUMABOX_META_DIR:-${KUMABOX_ROOT_DIR}/metadata}"
KUMABOX_BLOBS_DIR="${KUMABOX_BLOBS_DIR:-${KUMABOX_ROOT_DIR}/blobs}"
KUMABOX_TMP_DIR="${KUMABOX_TMP_DIR:-${KUMABOX_ROOT_DIR}/tmp}"
KUMABOX_RUN_DIR="${KUMABOX_RUN_DIR:-/run/kumabox}"
KUMABOX_LOG_DIR="${KUMABOX_LOG_DIR:-/var/log/kumabox}"
KUMABOX_CNI_CONF_DIR="${KUMABOX_CNI_CONF_DIR:-/etc/cni/net.d}"
KUMABOX_CNI_BIN_DIR="${KUMABOX_CNI_BIN_DIR:-/opt/cni/bin}"
# Overridable so the checks can be exercised without touching the real host.
KUMABOX_KVM_DEVICE="${KUMABOX_KVM_DEVICE:-/dev/kvm}"
KUMABOX_NETNS_DIR="${KUMABOX_NETNS_DIR:-/var/run/netns}"

# Dependency versions
CH_VERSION="${CH_VERSION:-v53.0}"
CH_MIN_MAJOR="${CH_MIN_MAJOR:-43}"
FW_VERSION="${FW_VERSION:-0.5.0}"
CNI_VERSION="${CNI_VERSION:-v1.9.1}"

# Architecture detection
ARCH=$(uname -m)
case "$ARCH" in
    x86_64|amd64)   GO_ARCH="amd64"; CH_SUFFIX=""; FW_SUFFIX="" ;;
    aarch64|arm64)  GO_ARCH="arm64"; CH_SUFFIX="-aarch64"; FW_SUFFIX="-aarch64" ;;
    *) echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

FIRMWARE_DIR="${KUMABOX_ROOT_DIR}/firmware"
FIRMWARE_PATH="${FIRMWARE_DIR}/CLOUDHV.fd"

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
                     hypervisor-fw    ${FW_VERSION}
                     CNI plugins      ${CNI_VERSION}
  --subnet=CIDR    Subnet for generated CNI bridge config (default: 10.88.0.0/16)

Environment variables:
  CH_VERSION      Cloud Hypervisor version   (default: ${CH_VERSION})
  CH_MIN_MAJOR    oldest accepted major      (default: ${CH_MIN_MAJOR})
  FW_VERSION      Firmware version           (default: ${FW_VERSION})
  CNI_VERSION     CNI plugins version        (default: ${CNI_VERSION})
  KUMABOX_ROOT_DIR / KUMABOX_RUN_DIR / KUMABOX_LOG_DIR
  KUMABOX_CNI_CONF_DIR / KUMABOX_CNI_BIN_DIR
  KUMABOX_KVM_DEVICE          KVM device to check   (default: /dev/kvm)
  KUMABOX_NETNS_DIR           network namespace dir (default: /var/run/netns)
EOF
            exit 0
            ;;
    esac
done

# ---------------------------------------------------------------------------
# Output helpers
# ---------------------------------------------------------------------------
PASS=0; WARN=0; FAIL=0

pass()   { PASS=$((PASS + 1)); printf "  \033[32m[PASS]\033[0m %s\n" "$1"; }
warn()   { WARN=$((WARN + 1)); printf "  \033[33m[WARN]\033[0m %s\n" "$1"; }
fail()   { FAIL=$((FAIL + 1)); printf "  \033[31m[FAIL]\033[0m %s\n" "$1"; }
info()   { printf "  \033[36m[INFO]\033[0m %s\n" "$1"; }
fixed()  { printf "  \033[32m[FIXED]\033[0m %s\n" "$1"; }
header() { printf "\n\033[1m==> %s\033[0m\n" "$1"; }

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
      "bridge": "kumabox0",
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
        qemu-img)   echo "qemu-utils" ;;
        *)          echo "" ;;
    esac
}

# KumaBox refuses to boot below this major version.
ch_version_ok() {
    local major
    major=$(echo "$1" | grep -oE 'v?[0-9]+' | head -1 | tr -d 'v')
    [ -n "$major" ] || return 1
    [ "$major" -ge "$CH_MIN_MAJOR" ]
}

# Layers are converted to EROFS at import time. erofs-utils < 1.8 tar mode
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
            qemu-img)         ver=$("$name" --version 2>/dev/null | head -1) || true ;;
            mkfs.ext4)        ver=$("$name" -V 2>&1 | head -1) || true ;;
            mkfs.erofs)       ver=$("$name" --version 2>&1 | head -1) || true ;;
        esac
        if [ "$name" = "cloud-hypervisor" ] && ! ch_version_ok "$ver"; then
            fail "$name (${ver:-unknown}) is older than v${CH_MIN_MAJOR} — kumabox refuses to launch on it"
            return
        fi
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
check_binary mkfs.erofs
check_binary mkfs.ext4
# qemu-img is optional: only the qcow2 write path needs it.
if command -v qemu-img &>/dev/null; then
    check_binary qemu-img
else
    warn "qemu-img not found (optional, needed by the qcow2 write path)"
fi

# ---------------------------------------------------------------------------
# 2. Firmware
# ---------------------------------------------------------------------------
header "Firmware"

# Only the UEFI boot shape needs firmware; direct kernel boot does not.
if [ -f "$FIRMWARE_PATH" ]; then
    local_size=$(stat -c%s "$FIRMWARE_PATH" 2>/dev/null || stat -f%z "$FIRMWARE_PATH" 2>/dev/null || echo 0)
    pass "CLOUDHV.fd (${local_size} bytes) at $FIRMWARE_PATH"
else
    warn "CLOUDHV.fd not found at $FIRMWARE_PATH (optional, needed by the uefi boot shape)"
fi

# ---------------------------------------------------------------------------
# 3. KVM access
# ---------------------------------------------------------------------------
header "KVM"

if [ -e "$KUMABOX_KVM_DEVICE" ]; then
    if [ -r "$KUMABOX_KVM_DEVICE" ] && [ -w "$KUMABOX_KVM_DEVICE" ]; then
        pass "$KUMABOX_KVM_DEVICE accessible"
    else
        fail "$KUMABOX_KVM_DEVICE exists but not readable/writable by $(whoami)"
        if $FIX; then
            chmod 666 "$KUMABOX_KVM_DEVICE" 2>/dev/null && fixed "chmod 666 $KUMABOX_KVM_DEVICE" || warn "failed to fix (need root?)"
        fi
    fi
else
    fail "$KUMABOX_KVM_DEVICE not found (nested virtualization or bare-metal required)"
fi

# ---------------------------------------------------------------------------
# 4. Runtime directories
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
check_dir "$KUMABOX_META_DIR"
check_dir "$KUMABOX_BLOBS_DIR"
check_dir "$KUMABOX_TMP_DIR"
check_dir "$KUMABOX_RUN_DIR"
check_dir "$KUMABOX_LOG_DIR"
check_dir "$FIRMWARE_DIR"
check_dir "$KUMABOX_NETNS_DIR"

# SQLite WAL needs coherent shared memory; report the same filesystem refusal
# enforced on open (docs/ARCHITECTURE.md §7).
meta_fstype=$(stat -f -c %T "$KUMABOX_ROOT_DIR" 2>/dev/null | head -1 | tr -d '[:space:]')
meta_fstype=${meta_fstype:-unknown}
case "$meta_fstype" in
    nfs*|cifs|smb*|fuse*)
        fail "root on $meta_fstype: sqlite WAL needs coherent shared memory; kumabox refuses this filesystem"
        ;;
    *)
        pass "root filesystem ($meta_fstype) supports WAL"
        ;;
esac

# ---------------------------------------------------------------------------
# 5. Sysctl
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
# 6. iptables FORWARD rules for the CNI bridge
# ---------------------------------------------------------------------------
header "iptables FORWARD (kumabox0)"

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

check_iptables_rule "FORWARD -i kumabox0 -j ACCEPT" \
    FORWARD -i kumabox0 -j ACCEPT
check_iptables_rule "FORWARD -o kumabox0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT" \
    FORWARD -o kumabox0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT

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
# 7. CNI configuration
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
# 8. CNI plugins
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
# 9. Store health
# ---------------------------------------------------------------------------
header "Store health"

DB_PATH="${KUMABOX_META_DIR}/kumabox.db"

if [ -f "$DB_PATH" ]; then
    pass "fact database present ($(stat -c%s "$DB_PATH" 2>/dev/null || echo '?') bytes)"
    if command -v sqlite3 &>/dev/null; then
        # Anything stuck mid-flight is what doctor is meant to surface; the
        # sweeper in 'kumabox gc' finishes or fails it (docs/BEHAVIOR.md §15).
        STUCK=$(sqlite3 "$DB_PATH" "select count(*) from images where state='importing'" 2>/dev/null || echo "?")
        if [ "$STUCK" = "?" ]; then
            warn "cannot read the images table (schema older than this build?)"
        elif [ "$STUCK" != "0" ]; then
            warn "$STUCK image import(s) left mid-flight; run 'kumabox gc' to finish or fail them"
        else
            pass "no image import left mid-flight"
        fi
    else
        warn "sqlite3 not found — skipping store inspection"
    fi
else
    info "no fact database yet (first command creates it)"
fi

# Staging leftovers are always worth reporting: they are disk usage nobody owns.
if [ -d "$KUMABOX_TMP_DIR" ]; then
    staging=$(find "$KUMABOX_TMP_DIR" -mindepth 1 -maxdepth 1 2>/dev/null | wc -l)
    if [ "$staging" -gt 0 ]; then
        warn "$staging staging entr(ies) in $KUMABOX_TMP_DIR; run 'kumabox gc' to reclaim them"
    else
        pass "no staging leftovers"
    fi
fi

# ---------------------------------------------------------------------------
# 10. Upgrade / Install
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

    # -- firmware -----------------------------------------------------------
    header "Install hypervisor-fw ${FW_VERSION}"

    fw_url="https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/${FW_VERSION}/hypervisor-fw${FW_SUFFIX}"
    mkdir -p "$FIRMWARE_DIR"
    info "downloading ${fw_url}"
    if curl -fsSL -o "${FIRMWARE_PATH}" "$fw_url"; then
        fixed "hypervisor-fw ${FW_VERSION} -> ${FIRMWARE_PATH}"
    else
        fail "failed to download firmware from ${fw_url}"
    fi

    # -- mkfs helpers -------------------------------------------------------
    for pkg_bin in "erofs-utils:mkfs.erofs" "e2fsprogs:mkfs.ext4"; do
        pkg="${pkg_bin%%:*}"; bin="${pkg_bin##*:}"
        if ! command -v "$bin" &>/dev/null; then
            header "Install ${pkg}"
            if command -v apt-get &>/dev/null; then
                apt-get install -y -qq "$pkg" &>/dev/null && fixed "${pkg} installed via apt-get" || warn "failed to install ${pkg}"
            elif command -v yum &>/dev/null; then
                yum install -y -q "$pkg" &>/dev/null && fixed "${pkg} installed via yum" || warn "failed to install ${pkg}"
            else
                warn "${pkg} not installed (install ${bin} manually)"
            fi
        fi
    done

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
printf "\n\033[1m--- Summary ---\033[0m\n"
printf "  Pass: %d  Warn: %d  Fail: %d\n\n" "$PASS" "$WARN" "$FAIL"

if [ "$FAIL" -gt 0 ] && ! $FIX; then
    info "Run '$0 --fix' to attempt automatic fixes"
    info "Run '$0 --upgrade' to install/upgrade cloud-hypervisor, firmware, and CNI plugins"
fi

[ "$FAIL" -eq 0 ] || exit 1
