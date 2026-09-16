#!/bin/sh
# KumaBox overlay-v1 initramfs root provider.
#
# The host attaches immutable EROFS disks as kumabox-layer0..N in manifest
# order and one ext4 disk as kumabox-cow. The kernel command line reverses the
# layer serials so OverlayFS sees the top layer first:
#
#   EROFS disks + ext4 COW
#            |
#            v
#   kumabox.layers=top,...,base  kumabox.cow=kumabox-cow
#            |                         |
#            +---- lowerdir list       +---- upper/work
#                           \           /
#                            overlay root

. /scripts/functions

# kumabox_device resolves one virtio block serial with a bounded wait. Device
# letters are deliberately ignored because VMM attachment order is not an ABI.
kumabox_device() {
	serial=$1
	attempt=0
	while [ "$attempt" -lt "$KUMABOX_DEVICE_TIMEOUT" ]; do
		for sysdev in /sys/block/*; do
			[ -d "$sysdev" ] || continue
			value=
			if [ -r "$sysdev/serial" ]; then
				value=$(cat "$sysdev/serial")
			elif [ -r "$sysdev/device/serial" ]; then
				value=$(cat "$sysdev/device/serial")
			fi
			if [ "$value" = "$serial" ]; then
				printf '/dev/%s\n' "${sysdev##*/}"
				return 0
			fi
		done
		sleep 1
		attempt=$((attempt + 1))
	done
	return 1
}

# mountroot is called by initramfs-tools when boot=kumabox-overlay is selected.
mountroot() {
	KUMABOX_LAYERS=
	KUMABOX_COW=
	KUMABOX_DEVICE_TIMEOUT=10
	for argument in $(cat /proc/cmdline); do
		case "$argument" in
			kumabox.layers=*) KUMABOX_LAYERS=${argument#kumabox.layers=} ;;
			kumabox.cow=*) KUMABOX_COW=${argument#kumabox.cow=} ;;
			kumabox.timeout=*) KUMABOX_DEVICE_TIMEOUT=${argument#kumabox.timeout=} ;;
		esac
	done

	case "$KUMABOX_DEVICE_TIMEOUT" in
		''|*[!0-9]*) panic "kumabox.timeout must be an integer" ;;
	esac
	[ "$KUMABOX_DEVICE_TIMEOUT" -gt 0 ] || panic "kumabox.timeout must be positive"
	[ -n "$KUMABOX_LAYERS" ] || panic "kumabox.layers is required"
	[ -n "$KUMABOX_COW" ] || panic "kumabox.cow is required"
	case "$KUMABOX_LAYERS" in
		,*|*,|*,,*) panic "kumabox.layers contains an empty serial" ;;
	esac
	case "$KUMABOX_COW" in
		*[!A-Za-z0-9_.-]*) panic "kumabox.cow contains an invalid serial" ;;
	esac

	modprobe erofs 2>/dev/null || true
	modprobe overlay 2>/dev/null || true
	modprobe ext4 2>/dev/null || true
	udevadm settle 2>/dev/null || true

	workspace=/.kumabox
	mkdir -p "$workspace/layers" "$workspace/cow"
	lowerdirs=
	old_ifs=$IFS
	IFS=,
	for serial in $KUMABOX_LAYERS; do
		case "$serial" in
			*[!A-Za-z0-9_.-]*) panic "kumabox.layers contains an invalid serial" ;;
		esac
		device=$(kumabox_device "$serial") || panic "KumaBox layer $serial was not found"
		mountpoint="$workspace/layers/$serial"
		mkdir -p "$mountpoint"
		mount -t erofs -o ro "$device" "$mountpoint" || panic "KumaBox layer $serial could not be mounted"
		if [ -n "$lowerdirs" ]; then
			lowerdirs="$lowerdirs:$mountpoint"
		else
			lowerdirs=$mountpoint
		fi
	done
	IFS=$old_ifs

	cow_device=$(kumabox_device "$KUMABOX_COW") || panic "KumaBox COW disk $KUMABOX_COW was not found"
	mount -t ext4 -o noatime "$cow_device" "$workspace/cow" || panic "KumaBox COW disk could not be mounted"
	mkdir -p "$workspace/cow/upper" "$workspace/cow/work"
	mount -t overlay overlay \
		-o "lowerdir=$lowerdirs,upperdir=$workspace/cow/upper,workdir=$workspace/cow/work" \
		"$rootmnt" || panic "KumaBox overlay root could not be mounted"

	mkdir -p "$rootmnt/dev" "$rootmnt/proc" "$rootmnt/sys" "$rootmnt/run" "$rootmnt/etc"
	rm -f "$rootmnt/etc/machine-id"
	: >"$rootmnt/etc/machine-id"
	log_success_msg "KumaBox overlay-v1 root is ready"
}
