#!/usr/bin/env bash
# scripts/kumabox-bridge.sh — create/delete the host bridge used by
# `kumabox run --bridge NAME`.
#
# KumaBox allocates each guest a static address from the bridge's IPv4 subnet;
# no DHCP server is required. The bridge is not persisted by this script.
#
# Usage:
#   sudo ./scripts/kumabox-bridge.sh create kubebr0 10.99.0.0/16
#   sudo ./scripts/kumabox-bridge.sh delete kubebr0

set -euo pipefail

PATH="${PATH:-}:/usr/local/sbin:/usr/sbin:/sbin"
export PATH

usage() {
    sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'
    exit 2
}

[ "$#" -eq 3 ] || [ "$#" -eq 2 ] || usage
action=$1
name=$2
cidr=${3:-}

case "$name" in
    ''|*[!A-Za-z0-9_.-]*|.|..)
        echo "invalid bridge name: $name" >&2
        exit 2
        ;;
esac
if [ "${#name}" -gt 15 ]; then
    echo "bridge name exceeds the Linux interface name limit: $name" >&2
    exit 2
fi

ensure_nat() {
    local subnet=$1
    if ! command -v nft >/dev/null 2>&1; then
        echo "nft is required for the host MASQUERADE rule" >&2
        exit 1
    fi
    if ! nft list table ip kumabox >/dev/null 2>&1; then
        nft add table ip kumabox
        nft 'add chain ip kumabox postrouting { type nat hook postrouting priority 100; }'
    fi
    if ! nft list table ip kumabox | grep -Fq "ip saddr ${subnet}"; then
        nft add rule ip kumabox postrouting ip saddr "$subnet" ip daddr != "$subnet" masquerade
    fi
    if command -v iptables >/dev/null 2>&1; then
        iptables -C FORWARD -i "$name" -j ACCEPT 2>/dev/null ||
            iptables -A FORWARD -i "$name" -j ACCEPT
        iptables -C FORWARD -o "$name" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null ||
            iptables -A FORWARD -o "$name" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
    fi
    sysctl -w net.ipv4.ip_forward=1 >/dev/null
}

case "$action" in
    create)
        [ -n "$cidr" ] || usage
        command -v python3 >/dev/null 2>&1 || { echo "python3 is required" >&2; exit 1; }
        read -r gateway prefix < <(python3 - "$cidr" <<'PY'
import ipaddress, sys
network = ipaddress.ip_network(sys.argv[1], strict=False)
if network.version != 4:
    raise SystemExit("bridge subnet must be IPv4")
print(next(network.hosts()), network.prefixlen)
PY
)
        if ! ip link show "$name" >/dev/null 2>&1; then
            ip link add "$name" type bridge
        fi
        ip addr replace "${gateway}/${prefix}" dev "$name"
        ip link set "$name" up
        ensure_nat "$cidr"
        echo "bridge ${name} is ready (${gateway}/${prefix}, subnet ${cidr})"
        echo "create a sandbox with: kumabox run IMAGE --bridge ${name}"
        ;;
    delete)
        ip link del "$name" 2>/dev/null || true
        if command -v iptables >/dev/null 2>&1; then
            iptables -D FORWARD -i "$name" -j ACCEPT 2>/dev/null || true
            iptables -D FORWARD -o "$name" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || true
        fi
        echo "bridge ${name} deleted; the shared MASQUERADE rule was left in place"
        ;;
    *)
        usage
        ;;
esac
