#!/bin/sh

PREREQ=""
prereqs() { echo "$PREREQ"; }
case "$1" in prereqs) prereqs; exit 0 ;; esac

[ -n "${rootmnt:-}" ] || exit 0

for arg in $(cat /proc/cmdline); do
    case "$arg" in
        kumabox.hostname=*) echo "${arg#kumabox.hostname=}" >"${rootmnt}/etc/hostname" ;;
    esac
done

dns_servers=""
has_static=false

for conf_file in /run/net-*.conf; do
    [ -f "$conf_file" ] || continue
    unset DEVICE IPV4ADDR IPV4NETMASK IPV4GATEWAY IPV4DNS0 IPV4DNS1 HWADDR
    . "$conf_file"
    [ -n "${DEVICE:-}" ] || continue
    [ -n "${IPV4ADDR:-}" ] || continue
    [ -n "${HWADDR:-}" ] || [ ! -e "/sys/class/net/${DEVICE}/address" ] || HWADDR="$(cat "/sys/class/net/${DEVICE}/address")"
    [ -n "${HWADDR:-}" ] || continue

    has_static=true
    prefix=0
    old_ifs="$IFS"
    IFS=.
    set -- $IPV4NETMASK
    IFS="$old_ifs"
    for octet in "$@"; do
        case "$octet" in
            255) prefix=$((prefix + 8)) ;;
            254) prefix=$((prefix + 7)) ;;
            252) prefix=$((prefix + 6)) ;;
            248) prefix=$((prefix + 5)) ;;
            240) prefix=$((prefix + 4)) ;;
            224) prefix=$((prefix + 3)) ;;
            192) prefix=$((prefix + 2)) ;;
            128) prefix=$((prefix + 1)) ;;
        esac
    done

    mac_name="$(echo "$HWADDR" | tr -d ':')"
    mkdir -p "${rootmnt}/etc/systemd/network"
    {
        printf "[Match]\nMACAddress=%s\n\n[Network]\nAddress=%s/%d\n" "$HWADDR" "$IPV4ADDR" "$prefix"
        [ -n "${IPV4GATEWAY:-}" ] && [ "$IPV4GATEWAY" != "0.0.0.0" ] && printf "Gateway=%s\n" "$IPV4GATEWAY"
        if [ -n "${IPV4DNS0:-}" ] && [ "$IPV4DNS0" != "0.0.0.0" ]; then
            printf "DNS=%s\n" "$IPV4DNS0"
            dns_servers="${dns_servers} ${IPV4DNS0}"
        fi
        if [ -n "${IPV4DNS1:-}" ] && [ "$IPV4DNS1" != "0.0.0.0" ]; then
            printf "DNS=%s\n" "$IPV4DNS1"
            dns_servers="${dns_servers} ${IPV4DNS1}"
        fi
    } >"${rootmnt}/etc/systemd/network/10-${mac_name}.network"
done

if [ "$has_static" = false ]; then
    mkdir -p "${rootmnt}/etc/systemd/network"
    for sysdev in /sys/class/net/*; do
        [ -e "$sysdev" ] || continue
        dev="${sysdev##*/}"
        case "$dev" in lo|bonding_masters) continue ;; esac
        [ -e "${sysdev}/address" ] || continue
        mac="$(cat "${sysdev}/address")"
        case "$mac" in ""|00:00:00:00:00:00) continue ;; esac
        mac_name="$(echo "$mac" | tr -d ':')"
        {
            printf "[Match]\nMACAddress=%s\n\n[Network]\nDHCP=ipv4\n\n[DHCPv4]\nClientIdentifier=mac\n" "$mac"
        } >"${rootmnt}/etc/systemd/network/10-${mac_name}.network"
    done
fi

[ -n "$dns_servers" ] || dns_servers="1.1.1.1 8.8.8.8"
: >"${rootmnt}/etc/resolv.conf"
for ns in $dns_servers; do
    printf "nameserver %s\n" "$ns" >>"${rootmnt}/etc/resolv.conf"
done
