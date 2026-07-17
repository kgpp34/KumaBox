#!/bin/sh

. /scripts/functions

resolve_disk() {
    serial="$1"
    timeout="${KUMABOX_TIMEOUT:-10}"
    i=0

    case "$serial" in
        /dev/*)
            while [ "$i" -lt "$timeout" ]; do
                [ -b "$serial" ] && echo "$serial" && return 0
                sleep 1
                i=$((i + 1))
            done
            return 1
            ;;
    esac

    while [ "$i" -lt "$timeout" ]; do
        for sysdev in /sys/block/vd*; do
            [ -d "$sysdev" ] || continue
            dev_serial=""
            [ -f "$sysdev/serial" ] && dev_serial="$(cat "$sysdev/serial")"
            [ -f "$sysdev/device/serial" ] && dev_serial="$(cat "$sysdev/device/serial")"
            dev_serial="$(printf '%s' "$dev_serial" | tr -d '[:space:]')"
            if [ "$dev_serial" = "$serial" ]; then
                echo "/dev/${sysdev##*/}"
                return 0
            fi
        done
        fallback="$(fallback_disk_by_order "$serial")"
        if [ -n "$fallback" ]; then
            echo "KumaBox: disk serial ${serial} not exposed; using attach-order fallback ${fallback}" >&2
            echo "$fallback"
            return 0
        fi
        sleep 1
        i=$((i + 1))
    done
    fallback="$(fallback_disk_by_order "$serial")"
    if [ -n "$fallback" ]; then
        echo "KumaBox: disk serial ${serial} not exposed; using attach-order fallback ${fallback}" >&2
        echo "$fallback"
        return 0
    fi
    dump_block_devices >&2
    return 1
}

ordinal_disk() {
    want="$1"
    idx=0
    for sysdev in /sys/block/vd*; do
        [ -d "$sysdev" ] || continue
        if [ "$idx" = "$want" ]; then
            echo "/dev/${sysdev##*/}"
            return 0
        fi
        idx=$((idx + 1))
    done
    return 1
}

layer_count() {
    count=0
    old_ifs="$IFS"
    IFS=,
    for _layer in ${LAYERS:-}; do
        count=$((count + 1))
    done
    IFS="$old_ifs"
    echo "$count"
}

fallback_disk_by_order() {
    serial="$1"
    case "$serial" in
        kumabox-layer*)
            idx="${serial#kumabox-layer}"
            case "$idx" in
                ''|*[!0-9]*) return 1 ;;
            esac
            ordinal_disk "$idx"
            return
            ;;
        kumabox-cow)
            ordinal_disk "$(layer_count)"
            return
            ;;
    esac
    return 1
}

dump_block_devices() {
    echo "KumaBox: available virtio block devices:"
    for sysdev in /sys/block/vd*; do
        [ -d "$sysdev" ] || continue
        dev_serial=""
        [ -f "$sysdev/serial" ] && dev_serial="$(cat "$sysdev/serial")"
        [ -f "$sysdev/device/serial" ] && dev_serial="$(cat "$sysdev/device/serial")"
        dev_serial="$(printf '%s' "$dev_serial" | tr -d '[:space:]')"
        size=""
        [ -f "$sysdev/size" ] && size="$(cat "$sysdev/size")"
        echo "KumaBox:   /dev/${sysdev##*/} serial=${dev_serial:-<empty>} sectors=${size:-unknown}"
    done
}

mountroot() {
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
    mkdir -p "${internal}/cow/upper" "${internal}/cow/work" "${internal}/cow/control"
    chmod 0700 "${internal}/cow/control"

    overlay_opts="lowerdir=${lower},upperdir=${internal}/cow/upper,workdir=${internal}/cow/work,index=on,redirect_dir=on,metacopy=on,xino=on"
    mount -t overlay overlay -o "$overlay_opts" "$rootmnt" || panic "overlay rootfs failed"

    mkdir -p "${rootmnt}/dev" "${rootmnt}/proc" "${rootmnt}/sys" "${rootmnt}/run"
    mkdir -p "${rootmnt}/.kumabox/cow"
    mount --bind "${internal}/cow/control" "${rootmnt}/.kumabox/cow" || panic "expose COW freeze mount failed"
    mount -o remount,bind,rw,nosuid,nodev,noexec "${rootmnt}/.kumabox/cow" || panic "secure COW freeze mount failed"

    for dev in $layer_devs; do
        blk="${dev##*/}"
        [ -e "/sys/block/${blk}/queue/scheduler" ] && echo none >"/sys/block/${blk}/queue/scheduler" 2>/dev/null || true
    done
    cow_blk="${cow_dev##*/}"
    [ -e "/sys/block/${cow_blk}/queue/scheduler" ] && echo mq-deadline >"/sys/block/${cow_blk}/queue/scheduler" 2>/dev/null || true

    rm -f "${rootmnt}/etc/machine-id" 2>/dev/null || true
    : >"${rootmnt}/etc/machine-id"

    log_success_msg "KumaBox: OCI overlay rootfs ready"
}
