#!/usr/bin/env bash
set -Eeuo pipefail

# End-to-end OCI boot and disk identity verification.
# The image must already be available in the local OCI daemon store.

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
kumabox_path="$repo_dir/bin/kumabox"
root_dir="/tmp/kumabox-p0/data"
run_dir="/tmp/kumabox-p0/run"
log_dir="/tmp/kumabox-p0/logs"
image_name="p6-agent-image"
image_ref="kumabox/ubuntu:24.04-p6"
network="${NETWORK:-cni:cocoon}"
vm_name="oci-disk-parity"
storage="64M"
console_copy="/tmp/kumabox-oci-disk-parity-console.log"

die() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

kb() {
	as_root "$kumabox_path" \
    --root-dir "$root_dir" \
    --run-dir "$run_dir" \
    --log-dir "$log_dir" \
    "$@"
}

as_root() {
	if [ "$(id -u)" -eq 0 ]; then
		"$@"
	else
		sudo "$@"
	fi
}

build_project() {
	if [ "$(id -u)" -eq 0 ]; then
		build_user="${SUDO_USER:-}"
		[ -n "$build_user" ] || die "run as a normal user or use sudo from a normal user"
		sudo -iu "$build_user" bash -lc "cd '$repo_dir' && make build"
		return
	fi
	make build
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "missing command: $1"
}

require_command jq
require_command make
require_command cloud-hypervisor
cd "$repo_dir"

printf '==> build host binary and Linux guest agent\n'
build_project

printf '==> remove old verification VM\n'
kb delete "$vm_name" --force >/dev/null 2>&1 || true

printf '==> rebuild managed OCI image\n'
kb image rm "$image_name" --force >/dev/null 2>&1 || true
kb image build "$image_ref" \
  --source daemon \
  --name "$image_name" \
  --platform linux/amd64 \
  --progress >/dev/null

printf '==> run VM\n'
run_json="$(kb run "$image_name" \
  --name "$vm_name" \
  --network "$network" \
  --storage "$storage")" || die "VM run failed"
printf '%s\n' "$run_json" | jq .

state="$(printf '%s\n' "$run_json" | jq -r '.state')"
console_log="$(printf '%s\n' "$run_json" | jq -r '.logDir')/console.log"
[ "$state" = running ] || die "VM state is $state"

printf '==> wait for guest agent\n'
kb agent ping "$vm_name" --timeout 120s | jq .

printf '==> preserve console log\n'
as_root cp "$console_log" "$console_copy"

printf '==> verify console boot flow\n'
grep -q 'KumaBox: mounting OCI overlay rootfs' "$console_copy" || die "overlay start log missing"
grep -q 'KumaBox: OCI overlay rootfs ready' "$console_copy" || die "overlay ready log missing"
if grep -Eq 'attach-order fallback|not exposed|serial .*not found|device .*not found|mount .* failed' "$console_copy"; then
  cat "$console_copy"
  die "disk lookup or mount error found in console"
fi

inspect_json="$(kb inspect "$vm_name" --json)"
printf '==> VM disk configuration\n'
printf '%s\n' "$inspect_json" | jq '.storageConfigs'

layer_count="$(printf '%s\n' "$inspect_json" | jq '[.storageConfigs[] | select(.role == "layer")] | length')"
cow_count="$(printf '%s\n' "$inspect_json" | jq '[.storageConfigs[] | select(.role == "cow")] | length')"
[ "$layer_count" -gt 0 ] || die "no OCI layer disks found"
[ "$cow_count" -eq 1 ] || die "expected one COW disk, got $cow_count"

printf '==> verify overlay root\n'
root_fs="$(kb exec "$vm_name" -- findmnt -n -o FSTYPE /)"
[ "$root_fs" = overlay ] || die "root filesystem is $root_fs, expected overlay"

printf '==> verify EROFS layers\n'
erofs_mounts="$(kb exec "$vm_name" -- findmnt -rn -t erofs)"
printf '%s\n' "$erofs_mounts"
erofs_count="$(printf '%s\n' "$erofs_mounts" | sed '/^[[:space:]]*$/d' | wc -l | tr -d ' ')"
[ "$erofs_count" -ge "$layer_count" ] || die "expected $layer_count EROFS mounts, got $erofs_count"

printf '==> verify writable COW\n'
cow_mount="$(kb exec "$vm_name" -- sh -c "findmnt -rn -t ext4 | grep '/.kumabox/cow' || true")"
[ -n "$cow_mount" ] || die "COW filesystem is not mounted"
printf '%s\n' "$cow_mount"

printf '==> verify virtio disk identities\n'
virtio_links="$(kb exec "$vm_name" -- sh -c 'ls -l /dev/disk/by-id/virtio-* 2>/dev/null || true')"
[ -n "$virtio_links" ] || die "guest has no virtio by-id disk links"
printf '%s\n' "$virtio_links"

while IFS= read -r serial; do
  [ -n "$serial" ] || continue
  kb exec "$vm_name" -- test -b "/dev/disk/by-id/virtio-$serial" || die "missing virtio-$serial"
  printf 'PASS: virtio-%s\n' "$serial"
done < <(
  printf '%s\n' "$inspect_json" |
    jq -r '.storageConfigs[] | select(.role == "layer" or .role == "cow") | .serial'
)

printf '==> verify guest execution\n'
hostname="$(kb exec "$vm_name" -- hostname)"
[ "$hostname" = "$vm_name" ] || die "unexpected guest hostname: $hostname"

printf '==> console log\n'
cat "$console_copy"

printf '==> delete verification VM\n'
kb delete "$vm_name" --force | jq .

printf '\nPASS: OCI boot, disk identity, overlay and COW verification completed\n'
printf 'console log: %s\n' "$console_copy"
