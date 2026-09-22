#!/bin/sh
# Persists initramfs static network facts into the assembled Ubuntu root.
#
#   kernel ip= parameters
#            |
#            v
#   /run/net-ethN.conf
#            |
#            v
#   MAC-matched systemd-networkd files in the writable overlay

PREREQ=""

prereqs() {
	printf '%s\n' "$PREREQ"
}

case "$1" in
prereqs)
	prereqs
	exit 0
	;;
esac

. /scripts/functions

[ -n "$rootmnt" ] || exit 0

for config_file in /run/net-*.conf; do
	[ -f "$config_file" ] || continue
	unset DEVICE IPV4ADDR IPV4NETMASK IPV4GATEWAY IPV4DNS0 IPV4DNS1 HWADDR
	. "$config_file"
	[ -n "$DEVICE" ] || continue
	[ -n "$IPV4ADDR" ] || continue

	if [ -z "$HWADDR" ] && [ -r "/sys/class/net/$DEVICE/address" ]; then
		HWADDR=$(cat "/sys/class/net/$DEVICE/address")
	fi
	[ -n "$HWADDR" ] || continue

	prefix=0
	old_ifs=$IFS
	IFS=.
	set -- $IPV4NETMASK
	IFS=$old_ifs
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

	identifier=$(printf '%s' "$HWADDR" | tr -d ':')
	directory="$rootmnt/etc/systemd/network"
	mkdir -p "$directory"
	{
		printf '[Match]\nMACAddress=%s\n\n' "$HWADDR"
		printf '[Network]\nAddress=%s/%s\n' "$IPV4ADDR" "$prefix"
		if [ -n "$IPV4GATEWAY" ] && [ "$IPV4GATEWAY" != "0.0.0.0" ]; then
			printf 'Gateway=%s\n' "$IPV4GATEWAY"
		fi
		if [ -n "$IPV4DNS0" ] && [ "$IPV4DNS0" != "0.0.0.0" ]; then
			printf 'DNS=%s\n' "$IPV4DNS0"
		fi
		if [ -n "$IPV4DNS1" ] && [ "$IPV4DNS1" != "0.0.0.0" ]; then
			printf 'DNS=%s\n' "$IPV4DNS1"
		fi
	} >"$directory/10-kumabox-$identifier.network"
done
