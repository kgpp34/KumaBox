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

# Docker supplies /etc/resolv.conf as a bind mount while building the image.
# The exported root can therefore contain only an empty placeholder. Point it
# at the resolver enabled in this guest without replacing a nonempty custom file.
resolver="$rootmnt/etc/resolv.conf"
if [ ! -L "$resolver" ] && [ ! -s "$resolver" ]; then
	rm -f "$resolver"
	ln -s /run/systemd/resolve/stub-resolv.conf "$resolver"
fi

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
		printf '[Network]\n'
		case "$IPV4ADDR" in
		169.254.*) printf 'DHCP=no\nLinkLocalAddressing=no\nIPv6AcceptRA=no\n' ;;
		*) printf 'Address=%s/%s\n' "$IPV4ADDR" "$prefix" ;;
		esac
		if [ -n "$IPV4GATEWAY" ] && [ "$IPV4GATEWAY" != "0.0.0.0" ]; then
			case "$IPV4ADDR" in 169.254.*) ;; *) printf 'Gateway=%s\n' "$IPV4GATEWAY" ;; esac
		fi
		if [ -n "$IPV4DNS0" ] && [ "$IPV4DNS0" != "0.0.0.0" ]; then
			printf 'DNS=%s\n' "$IPV4DNS0"
		fi
		if [ -n "$IPV4DNS1" ] && [ "$IPV4DNS1" != "0.0.0.0" ]; then
			printf 'DNS=%s\n' "$IPV4DNS1"
		fi
		case "$IPV4ADDR" in
		169.254.*)
			[ -z "$IPV4GATEWAY" ] || [ "$IPV4GATEWAY" = "0.0.0.0" ] || printf 'DNSDefaultRoute=yes\n'
			printf '\n[Address]\nAddress=%s/%s\nScope=global\nDuplicateAddressDetection=none\n' "$IPV4ADDR" "$prefix"
			[ -z "$IPV4GATEWAY" ] || [ "$IPV4GATEWAY" = "0.0.0.0" ] || printf '\n[Route]\nDestination=0.0.0.0/0\nGateway=%s\nPreferredSource=%s\n' "$IPV4GATEWAY" "$IPV4ADDR"
			;;
		esac
	} >"$directory/10-kumabox-$identifier.network"
	case "$IPV4ADDR" in
	169.254.*)
		if [ -n "$IPV4GATEWAY" ] && [ "$IPV4GATEWAY" != 0.0.0.0 ] && [ -n "$IPV4DNS0" ] && [ "$IPV4DNS0" != 0.0.0.0 ]; then
			mkdir -p "$rootmnt/etc/kumabox"
			{ printf 'nameserver %s\n' "$IPV4DNS0"; [ -z "$IPV4DNS1" ] || [ "$IPV4DNS1" = 0.0.0.0 ] || printf 'nameserver %s\n' "$IPV4DNS1"; } > "$rootmnt/etc/kumabox/resolv.conf"
			rm -f "$resolver"
			ln -s /etc/kumabox/resolv.conf "$resolver"
		fi
		;;
	esac
done
