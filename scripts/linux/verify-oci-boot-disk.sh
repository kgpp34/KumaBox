#!/usr/bin/env bash
set -Eeuo pipefail

# End-to-end OCI boot and disk identity verification.
# The image must already be available in the local OCI daemon store.

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
kumabox_path="$repo_dir/bin/kumabox"
cloud_hypervisor_path="cloud-hypervisor"
qemu_img_path="qemu-img"
root_dir="/var/lib/kumabox"
run_dir="/run/kumabox"
log_dir="/var/log/kumabox"
image_name="p6-agent-image-verify-$(date +%s)"
image_ref="kumabox/ubuntu:24.04-p6"
network="${NETWORK:-cni:cocoon}"
vm_name="oci-disk-parity"
storage="64M"
console_copy="/tmp/kumabox-oci-disk-parity-console.log"
console_pid=
console_input_pid=
console_tmp_dir=
metadata_backend=json
metadata_path=
skip_build=false
skip_image_build=false
remove_image=false

die() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

kb() {
	local metadata_args=(--metadata-backend "$metadata_backend")
	if [ -n "$metadata_path" ]; then
		metadata_args+=(--metadata-path "$metadata_path")
	fi
	as_root "$kumabox_path" \
    --root-dir "$root_dir" \
    --run-dir "$run_dir" \
    --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor_path" \
    --qemu-img-bin "$qemu_img_path" \
    "${metadata_args[@]}" \
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

require_value() {
	[ -n "${2:-}" ] || die "$1 requires a value"
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--kumabox) require_value "$1" "${2:-}"; kumabox_path="$2"; shift 2 ;;
	--cloud-hypervisor) require_value "$1" "${2:-}"; cloud_hypervisor_path="$2"; shift 2 ;;
	--qemu-img) require_value "$1" "${2:-}"; qemu_img_path="$2"; shift 2 ;;
	--root-dir) require_value "$1" "${2:-}"; root_dir="$2"; shift 2 ;;
	--run-dir) require_value "$1" "${2:-}"; run_dir="$2"; shift 2 ;;
	--log-dir) require_value "$1" "${2:-}"; log_dir="$2"; shift 2 ;;
	--image-name) require_value "$1" "${2:-}"; image_name="$2"; shift 2 ;;
	--image-ref) require_value "$1" "${2:-}"; image_ref="$2"; shift 2 ;;
	--network) require_value "$1" "${2:-}"; network="$2"; shift 2 ;;
	--name) require_value "$1" "${2:-}"; vm_name="$2"; shift 2 ;;
	--storage) require_value "$1" "${2:-}"; storage="$2"; shift 2 ;;
	--metadata-backend) require_value "$1" "${2:-}"; metadata_backend="$2"; shift 2 ;;
	--metadata-path) require_value "$1" "${2:-}"; metadata_path="$2"; shift 2 ;;
	--skip-build) skip_build=true; shift ;;
	--skip-image-build) skip_image_build=true; shift ;;
	-h|--help)
		printf '%s\n' 'Usage: verify-oci-boot-disk.sh [options]' \
			'  --kumabox PATH --cloud-hypervisor PATH --qemu-img PATH' \
			'  --root-dir PATH --run-dir PATH --log-dir PATH' \
			'  --image-name NAME --image-ref REF --network NETWORK --name VM' \
			'  --storage SIZE --metadata-backend json|sqlite --metadata-path PATH' \
			'  --skip-build --skip-image-build'
		exit 0
		;;
	*) die "unknown option: $1" ;;
	esac
done

[ "$metadata_backend" = json ] || [ "$metadata_backend" = sqlite ] || die "metadata backend must be json or sqlite"

print_run_failure_context() {
	set +e
	printf '\n==> failure context: VM inspect\n' >&2
	failed_json="$(kb inspect "$vm_name" --json 2>/dev/null)"
	printf '%s\n' "$failed_json" | jq . >&2
	failed_log_dir="$(printf '%s\n' "$failed_json" | jq -r '.logDir // empty')"
	if [ -n "$failed_log_dir" ]; then
		printf '\n==> failure context: console tail\n' >&2
		as_root tail -n 160 "$failed_log_dir/console.log" 2>/dev/null >&2 || true
		printf '\n==> failure context: VMM stderr\n' >&2
		as_root tail -n 120 "$failed_log_dir/cloud-hypervisor.stderr.log" 2>/dev/null >&2 || true
	fi
}

stop_console_capture() {
	if [ -n "$console_pid" ]; then
		kill "$console_pid" >/dev/null 2>&1 || true
		wait "$console_pid" >/dev/null 2>&1 || true
		console_pid=
	fi
	if [ -n "$console_input_pid" ]; then
		kill "$console_input_pid" >/dev/null 2>&1 || true
		wait "$console_input_pid" >/dev/null 2>&1 || true
		console_input_pid=
	fi
	if [ -n "$console_tmp_dir" ]; then
		rm -rf "$console_tmp_dir"
		console_tmp_dir=
	fi
}

capture_console() {
	console_tmp_dir="$(mktemp -d /tmp/kumabox-console.XXXXXX)"
	local fifo="$console_tmp_dir/input"
	mkfifo "$fifo"
	# Keep stdin open so the console relay remains attached while the guest boots.
	tail -f /dev/null >"$fifo" &
	console_input_pid=$!
	kb console "$vm_name" <"$fifo" >"$console_copy" 2>"$console_tmp_dir/stderr" &
	console_pid=$!
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "missing command: $1"
}

require_command jq
require_command "$cloud_hypervisor_path"
require_command "$qemu_img_path"
cd "$repo_dir"
trap stop_console_capture EXIT

printf '==> build host binary and Linux guest agent\n'
if [ "$skip_build" = true ]; then
	printf '%s\n' 'state: using current host binary and guest agent'
else
	require_command make
	build_project
fi

printf '==> remove old verification VM\n'
kb delete "$vm_name" --force >/dev/null 2>&1 || true

if [ "$skip_image_build" = true ]; then
	printf '==> use existing managed OCI image\n'
	kb image inspect "$image_name" --json >/dev/null || die "managed image not found: $image_name"
else
	printf '==> rebuild managed OCI image\n'
	kb image build "$image_ref" \
		--source daemon \
		--name "$image_name" \
		--platform linux/amd64 \
		--progress >/dev/null
	remove_image=true
fi

printf '==> run VM\n'
run_json="$(kb run "$image_name" \
  --name "$vm_name" \
  --network "$network" \
  --storage "$storage")" || {
  print_run_failure_context
  die "VM run failed; inspect the failure context above"
}
printf '%s\n' "$run_json" | jq .

state="$(printf '%s\n' "$run_json" | jq -r '.state')"
console_log="$(printf '%s\n' "$run_json" | jq -r '.logDir')/console.log"
[ "$state" = running ] || die "VM state is $state"

printf '==> capture live console\n'
capture_console

printf '==> wait for guest agent\n'
kb agent ping "$vm_name" --timeout 120s | jq .
stop_console_capture

printf '==> preserve console log\n'
if [ -f "$console_log" ]; then
	as_root cp "$console_log" "$console_copy"
	as_root chmod 0644 "$console_copy"
fi

printf '==> console boot log\n'
cat "$console_copy"

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

printf '==> verify EROFS layers through overlay lowerdir\n'
root_mount="$(kb exec "$vm_name" -- findmnt -n -o TARGET,OPTIONS /)"
printf '%s\n' "$root_mount"
lowerdir="$(printf '%s\n' "$root_mount" | sed -n 's/.*lowerdir=\([^,]*\).*/\1/p')"
[ -n "$lowerdir" ] || die "overlay lowerdir is missing"

for layer_index in $(seq 0 $((layer_count - 1))); do
	layer_path="/.kumabox/layers/kumabox-layer${layer_index}"
	case ":$lowerdir:" in
		*":$layer_path:"*) printf 'PASS: %s is in overlay lowerdir\n' "$layer_path" ;;
		*) die "missing $layer_path from overlay lowerdir" ;;
	esac
done

printf '==> verify writable COW\n'
kb exec "$vm_name" -- sh -c \
  'test -w / && touch /.kumabox-cow-verify && test -f /.kumabox-cow-verify && rm -f /.kumabox-cow-verify' \
  || die "overlay root is not writable through COW"
printf '%s\n' 'PASS: writable COW is active through overlay root'

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

printf '==> delete verification VM\n'
kb delete "$vm_name" --force | jq .

if [ "$remove_image" = true ]; then
	printf '==> remove temporary verification image\n'
	kb image rm "$image_name" >/dev/null
fi

printf '\nPASS: OCI boot, disk identity, overlay and COW verification completed\n'
printf 'console log: %s\n' "$console_copy"
