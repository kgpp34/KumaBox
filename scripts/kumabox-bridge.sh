#!/usr/bin/env bash
# scripts/kumabox-bridge.sh — create/delete the host bridge used by
# `kumabox run --bridge NAME`.
#
# The default guest mode is DHCP, matching Cocoon's bridge mode. Pass --static
# to use KumaBox's host-side static IPAM instead (`--bridge-static`).
#
# Usage:
#   sudo ./scripts/kumabox-bridge.sh create kubebr0 10.99.0.0/16
#   sudo ./scripts/kumabox-bridge.sh create kubebr0 10.99.0.0/16 --static
#   sudo ./scripts/kumabox-bridge.sh delete kubebr0
#
# The bridge and DHCP server are not persisted by this script; use
# systemd-networkd/dnsmasq units for production.

set -euo pipefail

PATH="${PATH:-}:/usr/local/sbin:/usr/sbin:/sbin"
export PATH

usage() {
    sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'
    exit 2
}

[ "$#" -ge 2 ] || usage
action=$1
name=$2
cidr=${3:-}
mode=${4:---dhcp}

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

pid_file="/run/kumabox-bridge-${name}.pid"

stop_dhcp() {
    if command -v systemctl >/dev/null 2>&1; then
        systemctl disable --now "kumabox-bridge-dhcp@${name}.service" 2>/dev/null || true
    fi
    if [ -f "$pid_file" ]; then
        local pid
        pid=$(cat "$pid_file" 2>/dev/null || true)
        if [ -n "$pid" ]; then
            kill "$pid" 2>/dev/null || true
        fi
        rm -f "$pid_file"
    fi
}

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

start_dhcp() {
    local subnet=$1
    command -v dnsmasq >/dev/null 2>&1 || {
        echo "dnsmasq is required for --dhcp; install it or use --static" >&2
        exit 1
    }
    command -v python3 >/dev/null 2>&1 || { echo "python3 is required" >&2; exit 1; }
    local gateway netmask start end
    read -r gateway netmask start end < <(python3 - "$subnet" <<'PY'
import ipaddress, sys
network = ipaddress.ip_network(sys.argv[1], strict=False)
if network.version != 4:
    raise SystemExit("bridge subnet must be IPv4")
hosts = network.hosts()
gateway = next(hosts)
start = next(hosts, gateway)
end = network.broadcast_address - 1
print(gateway, network.netmask, start, end)
PY
)
    stop_dhcp
    local range="${start},${end},${netmask},12h"
    if command -v systemctl >/dev/null 2>&1; then
        mkdir -p /etc/kumabox
        cat > "/etc/kumabox/bridge-${name}.env" <<EOF
DHCP_RANGE=${range}
GATEWAY=${gateway}
EOF
        cat > /etc/systemd/system/kumabox-bridge-dhcp@.service <<'UNIT'
[Unit]
Description=KumaBox DHCP for bridge %i
After=network.target

[Service]
Type=simple
EnvironmentFile=/etc/kumabox/bridge-%i.env
ExecStart=/usr/sbin/dnsmasq --keep-in-foreground --interface=%i --bind-interfaces --dhcp-range=${DHCP_RANGE} --dhcp-option=3,${GATEWAY} --dhcp-option=6,8.8.8.8,1.1.1.1 --no-ping --port=0

[Install]
WantedBy=multi-user.target
UNIT
        systemctl daemon-reload
        if systemctl enable --now "kumabox-bridge-dhcp@${name}.service"; then
            echo "DHCP unit started on ${name} (${start}-${end}, gateway ${gateway})"
            return
        fi
        echo "systemd DHCP unit failed; falling back to a foreground process" >&2
    fi
    dnsmasq --interface="$name" --bind-interfaces \
        --dhcp-range="${range}" \
        --dhcp-option=3,"${gateway}" \
        --dhcp-option=6,8.8.8.8,1.1.1.1 --no-ping \
        --pid-file="$pid_file" --port=0
    echo "DHCP server started on ${name} (${start}-${end}, gateway ${gateway})"
}

case "$action" in
    create)
        [ -n "$cidr" ] || usage
        case "$mode" in
            --dhcp|--static) ;;
            *) echo "unknown bridge mode: $mode" >&2; usage ;;
        esac
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
        if [ "$mode" = "--dhcp" ]; then
            start_dhcp "$cidr"
            echo "create a sandbox with: kumabox run IMAGE --bridge ${name}"
        else
            echo "create a sandbox with: kumabox run IMAGE --bridge ${name} --bridge-static"
        fi
        echo "bridge ${name} is ready (${gateway}/${prefix}, subnet ${cidr})"
        ;;
    delete)
        stop_dhcp
        rm -f "/etc/kumabox/bridge-${name}.env"
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
