#!/bin/sh

. /scripts/functions

boot_phase() {
    phase="$1"
    phase_dir=/run/kumabox
    uptime="$(cut -d' ' -f1 /proc/uptime 2>/dev/null || true)"
    seconds="${uptime%%.*}"
    fraction="${uptime#*.}"
    [ "$seconds" != "$uptime" ] || seconds=0
    [ -n "$fraction" ] || fraction=0
    fraction="$(printf '%s000' "$fraction" | cut -c1-3)"
    mkdir -p "$phase_dir"
    printf 'KumaBox: boot-phase=%s monotonic-ms=%s\n' \
        "$phase" "$((seconds * 1000 + fraction))" >>"$phase_dir/boot-phases"
}

resolve_disk() {
    serial="$1"
    timeout="${KUMABOX_TIMEOUT:-10}"
    i=0

    case "$timeout" in
        ''|*[!0-9]*) timeout=10 ;;
    esac

    case "$serial" in
        /dev/*)
            while [ "$i" -lt "$timeout" ]; do
                [ -b "$serial" ] && echo "$serial" && return 0
                sleep 1
                i=$((i + 1))
            done
            echo "KumaBox: device ${serial} not present after ${timeout}s" >&2
            return 1
            ;;
    esac

    while [ "$i" -lt "$timeout" ]; do
        for sysdev in /sys/block/vd*; do
            [ -d "$sysdev" ] || continue
            dev_serial=""
            if [ -f "$sysdev/serial" ]; then
                dev_serial="$(cat "$sysdev/serial")"
            fi
            if [ -f "$sysdev/device/serial" ]; then
                dev_serial="$(cat "$sysdev/device/serial")"
            fi
            while :; do
                case "$dev_serial" in
                    *[[:space:]]) dev_serial="${dev_serial%[[:space:]]}" ;;
                    *) break ;;
                esac
            done
            if [ "$dev_serial" = "$serial" ]; then
                echo "/dev/${sysdev##*/}"
                return 0
            fi
        done
        sleep 1
        i=$((i + 1))
    done
    echo "KumaBox: device serial ${serial} not found; available virtio disks:" >&2
    for sysdev in /sys/block/vd*; do
        [ -d "$sysdev" ] || continue
        dev_serial=""
        [ -f "$sysdev/serial" ] && dev_serial="$(cat "$sysdev/serial")"
        [ -f "$sysdev/device/serial" ] && dev_serial="$(cat "$sysdev/device/serial")"
        echo "KumaBox: ${sysdev##*/} serial=${dev_serial}" >&2
    done
    return 1
}

mountroot() {
    boot_phase overlay-start
    log_begin_msg "KumaBox: mounting OCI overlay rootfs"

    if ! ls /run/net-*.conf >/dev/null 2>&1; then
        for arg in $(cat /proc/cmdline); do
            case "$arg" in
                ip=*) configure_networking; break ;;
            esac
        done
    fi

    modprobe erofs 2>/dev/null || true
    modprobe overlay 2>/dev/null || true
    modprobe ext4 2>/dev/null || true

    for arg in $(cat /proc/cmdline); do
        case "$arg" in
            kumabox.layers=*) LAYERS="${arg#kumabox.layers=}" ;;
            kumabox.cow=*) COW="${arg#kumabox.cow=}" ;;
            kumabox.timeout=*) KUMABOX_TIMEOUT="${arg#kumabox.timeout=}" ;;
        esac
    done

    [ -n "${LAYERS:-}" ] || panic "kumabox.layers= not set"
    [ -n "${COW:-}" ] || panic "kumabox.cow= not set"

    udevadm settle 2>/dev/null || true

    internal="/.kumabox"
    mkdir -p "$internal"

    lower=""
    layer_devs=""
    old_ifs="$IFS"
    IFS=,
    for serial in $LAYERS; do
        dev="$(resolve_disk "$serial")" || panic "layer device ${serial} not found"
        mnt="${internal}/layers/${serial}"
        mkdir -p "$mnt"
        mount -t erofs -o ro "$dev" "$mnt" || panic "mount layer ${serial} failed"
        [ -n "$lower" ] && lower="${lower}:"
        lower="${lower}${mnt}"
        layer_devs="${layer_devs} ${dev}"
    done
    IFS="$old_ifs"

    cow_dev="$(resolve_disk "$COW")" || panic "COW device ${COW} not found"
    mkdir -p "${internal}/cow"
    mount -t ext4 -o noatime "$cow_dev" "${internal}/cow" || panic "mount COW failed"
    mkdir -p "${internal}/cow/upper" "${internal}/cow/work"

    overlay_opts="lowerdir=${lower},upperdir=${internal}/cow/upper,workdir=${internal}/cow/work,index=on,redirect_dir=on,metacopy=on,xino=on"
    mount -t overlay overlay -o "$overlay_opts" "$rootmnt" || panic "overlay rootfs failed"

    mkdir -p "${rootmnt}/dev" "${rootmnt}/proc" "${rootmnt}/sys" "${rootmnt}/run"
    for dev in $layer_devs; do
        blk="${dev##*/}"
        [ -e "/sys/block/${blk}/queue/scheduler" ] && echo none >"/sys/block/${blk}/queue/scheduler" 2>/dev/null || true
    done
    cow_blk="${cow_dev##*/}"
    [ -e "/sys/block/${cow_blk}/queue/scheduler" ] && echo mq-deadline >"/sys/block/${cow_blk}/queue/scheduler" 2>/dev/null || true

    boot_phase overlay-ready
    log_success_msg "KumaBox: OCI overlay rootfs ready"
}
