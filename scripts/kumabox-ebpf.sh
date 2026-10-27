#!/usr/bin/env bash
# Install shared IPv4 forwarding/NAT rules for the TAP/eBPF provider.
# Usage: sudo kumabox-ebpf setup|check|remove [SUBNET] [TAP_PREFIX]
# Defaults match network.ebpf.subnet and an empty network.scope.
# Per-sandbox address translation and isolation run in TC eBPF. Linux routing
# and conntrack provide external NAT; there are no per-sandbox firewall rules.
set -euo pipefail
PATH="${PATH:-}:/usr/local/sbin:/usr/sbin:/sbin"
export PATH

action=${1:-check}
subnet=${2:-10.89.0.0/16}
prefix=${3:-kt}
[[ "$prefix" =~ ^[a-zA-Z0-9]{1,5}$ ]] || { echo 'invalid TAP prefix' >&2; exit 2; }
[[ "$subnet" =~ ^[0-9.]+/[0-9]+$ ]] || { echo 'invalid IPv4 subnet' >&2; exit 2; }
command -v python3 >/dev/null
python3 - "$subnet" <<'PY'
import ipaddress, sys
pool = ipaddress.ip_network(sys.argv[1], strict=True)
private_ranges = [ipaddress.ip_network(cidr) for cidr in ('10.0.0.0/8', '172.16.0.0/12', '192.168.0.0/16')]
if pool.version != 4 or not 16 <= pool.prefixlen <= 28 or not any(pool.subnet_of(private) for private in private_ranges):
    raise SystemExit('subnet must be private IPv4 /16 through /28')
PY
command -v iptables >/dev/null
[[ $EUID == 0 ]] || { echo 'run as root' >&2; exit 1; }

comment="kumabox-ebpf:${prefix}:${subnet}"
outbound=(-i "${prefix}+" -s "$subnet" -m comment --comment "$comment" -j ACCEPT)
inbound=(-o "${prefix}+" -d "$subnet" -m conntrack --ctstate RELATED,ESTABLISHED -m comment --comment "$comment" -j ACCEPT)
legacy_nat=(-s "$subnet" '!' -d "$subnet" -m comment --comment "$comment" -j MASQUERADE)
nat=("${legacy_nat[@]}" --random-fully)
case "$action" in
    setup)
        mountpoint -q /sys/fs/bpf || mount -t bpf bpf /sys/fs/bpf
        sysctl -w net.ipv4.ip_forward=1 >/dev/null
        iptables -w -C FORWARD "${outbound[@]}" 2>/dev/null || iptables -w -I FORWARD 1 "${outbound[@]}"
        iptables -w -C FORWARD "${inbound[@]}" 2>/dev/null || iptables -w -I FORWARD 1 "${inbound[@]}"
        iptables -w -t nat -C POSTROUTING "${nat[@]}" 2>/dev/null || iptables -w -t nat -A POSTROUTING "${nat[@]}"
        if iptables -w -t nat -C POSTROUTING "${legacy_nat[@]}" 2>/dev/null; then iptables -w -t nat -D POSTROUTING "${legacy_nat[@]}"; fi
        ;;
    check)
        mountpoint -q /sys/fs/bpf
        [[ $(sysctl -n net.ipv4.ip_forward) == 1 ]]
        iptables -w -C FORWARD "${outbound[@]}"
        iptables -w -C FORWARD "${inbound[@]}"
        iptables -w -t nat -C POSTROUTING "${nat[@]}"
        ;;
    remove)
        if iptables -w -C FORWARD "${outbound[@]}" 2>/dev/null; then iptables -w -D FORWARD "${outbound[@]}"; fi
        if iptables -w -C FORWARD "${inbound[@]}" 2>/dev/null; then iptables -w -D FORWARD "${inbound[@]}"; fi
        if iptables -w -t nat -C POSTROUTING "${nat[@]}" 2>/dev/null; then iptables -w -t nat -D POSTROUTING "${nat[@]}"; fi
        ;;
    *) echo 'usage: kumabox-ebpf setup|check|remove [SUBNET] [TAP_PREFIX]' >&2; exit 2 ;;
esac
printf 'TAP/eBPF host rules: %s (%s, %s)\n' "$action" "$subnet" "$prefix"
