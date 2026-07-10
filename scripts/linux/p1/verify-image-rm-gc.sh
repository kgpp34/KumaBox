#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
root_dir="/tmp/kumabox-p0/data"
run_dir="/tmp/kumabox-p0/run"
log_dir="/tmp/kumabox-p0/logs"
cloud_hypervisor_path="cloud-hypervisor"
qemu_img_path="qemu-img"
image_name="p1-rm-gc"
vm_name="p1-ref"
root_disk=""
firmware=""

usage() {
  cat <<'USAGE'
Usage: scripts/linux/p1/verify-image-rm-gc.sh --root-disk PATH --firmware PATH [options]

Options:
  --kumabox PATH             kumabox binary path, defaults to ./bin/kumabox
  --cloud-hypervisor PATH    cloud-hypervisor binary path, defaults to cloud-hypervisor
  --qemu-img PATH            qemu-img binary path, defaults to qemu-img
  --root-dir PATH            store directory, defaults to /tmp/kumabox-p0/data
  --run-dir PATH             runtime directory, defaults to /tmp/kumabox-p0/run
  --log-dir PATH             log directory, defaults to /tmp/kumabox-p0/logs
  --image-name NAME          image name, defaults to p1-rm-gc
  --name NAME                VM name, defaults to p1-ref
  --root-disk PATH           cloud image root disk path
  --firmware PATH            UEFI firmware path for cloud-image boot

Verifies P1-06/P1-07:
clean data/run/logs -> image import -> create IMAGE -> image rm must fail
with IMAGE_IN_USE -> gc dry-run reports staging/orphan but not active image
-> delete VM -> image rm succeeds -> clean data/run/logs.
The fixtures directory is never removed.
USAGE
}

require_value() {
  local flag="$1"
  local value="${2:-}"
  if [[ -z "$value" ]]; then
    echo "$flag requires a value" >&2
    exit 2
  fi
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --kumabox)
      require_value "$1" "${2:-}"
      kumabox_path="$2"
      shift 2
      ;;
    --cloud-hypervisor)
      require_value "$1" "${2:-}"
      cloud_hypervisor_path="$2"
      shift 2
      ;;
    --qemu-img)
      require_value "$1" "${2:-}"
      qemu_img_path="$2"
      shift 2
      ;;
    --root-dir)
      require_value "$1" "${2:-}"
      root_dir="$2"
      shift 2
      ;;
    --run-dir)
      require_value "$1" "${2:-}"
      run_dir="$2"
      shift 2
      ;;
    --log-dir)
      require_value "$1" "${2:-}"
      log_dir="$2"
      shift 2
      ;;
    --image-name)
      require_value "$1" "${2:-}"
      image_name="$2"
      shift 2
      ;;
    --name)
      require_value "$1" "${2:-}"
      vm_name="$2"
      shift 2
      ;;
    --root-disk)
      require_value "$1" "${2:-}"
      root_disk="$2"
      shift 2
      ;;
    --firmware)
      require_value "$1" "${2:-}"
      firmware="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ -z "$root_disk" || -z "$firmware" ]]; then
  usage >&2
  exit 2
fi

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "verify-image-rm-gc must run inside the Linux VM" >&2
  exit 1
fi

if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required" >&2
  exit 1
fi

for path in "$kumabox_path" "$root_disk" "$firmware"; do
  if [[ ! -e "$path" ]]; then
    echo "required path does not exist: $path" >&2
    exit 1
  fi
done

if [[ ! -x "$kumabox_path" ]]; then
  echo "kumabox is not executable: $kumabox_path" >&2
  exit 1
fi

vm_exists=0
cleanup() {
  set +e
  if [[ "$vm_exists" -eq 1 ]]; then
    "$kumabox_path" --root-dir "$root_dir" --run-dir "$run_dir" --log-dir "$log_dir" --cloud-hypervisor-bin "$cloud_hypervisor_path" delete "$vm_name" --force >/dev/null 2>&1
  fi
  rm -rf "$root_dir" "$run_dir" "$log_dir"
}
trap cleanup EXIT

rm -rf "$root_dir" "$run_dir" "$log_dir"
mkdir -p "$root_dir" "$run_dir" "$log_dir"

scripts/linux/env-check.sh \
  --kumabox "$kumabox_path" \
  --cloud-hypervisor "$cloud_hypervisor_path" \
  --qemu-img "$qemu_img_path" \
  --strict

image_json="$("$kumabox_path" \
  --root-dir "$root_dir" \
  image import "$root_disk" \
  --name "$image_name" \
  --firmware "$firmware" \
  --qemu-img "$qemu_img_path")"
printf '%s\n' "$image_json"

image_id="$(printf '%s' "$image_json" | jq -r '.id')"
image_root_disk="$(printf '%s' "$image_json" | jq -r '.rootDisk.path')"
image_dir="$(dirname "$image_root_disk")"
if [[ -z "$image_id" || "$image_id" == "null" || ! -f "$image_root_disk" ]]; then
  echo "image import did not produce a managed root disk" >&2
  exit 1
fi

created_json="$("$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  create "$image_name" \
  --name "$vm_name")"
vm_exists=1
printf '%s\n' "$created_json"

if "$kumabox_path" --root-dir "$root_dir" image rm "$image_name" >/tmp/kumabox-image-rm.err 2>&1; then
  echo "image rm unexpectedly succeeded while VM references the image" >&2
  exit 1
fi
if ! grep -q "IMAGE_IN_USE" /tmp/kumabox-image-rm.err; then
  echo "image rm did not report IMAGE_IN_USE" >&2
  cat /tmp/kumabox-image-rm.err >&2
  exit 1
fi
echo "pass: image rm rejects referenced image"

staging_dir="$root_dir/cloudimg/staging/import-leftover"
orphan_dir="$root_dir/cloudimg/img_orphan"
mkdir -p "$staging_dir" "$orphan_dir"

gc_json="$("$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  gc --dry-run --json)"
printf '%s\n' "$gc_json"

if [[ "$(printf '%s' "$gc_json" | jq --arg path "$staging_dir" '[.candidates[] | select(.component == "image" and .type == "image_staging_dir" and .path == $path)] | length')" != "1" ]]; then
  echo "gc did not report image staging candidate" >&2
  exit 1
fi
if [[ "$(printf '%s' "$gc_json" | jq --arg path "$orphan_dir" '[.candidates[] | select(.component == "image" and .type == "orphan_image_dir" and .path == $path)] | length')" != "1" ]]; then
  echo "gc did not report orphan image dir candidate" >&2
  exit 1
fi
if [[ "$(printf '%s' "$gc_json" | jq --arg path "$image_dir" '[.candidates[] | select(.path == $path)] | length')" != "0" ]]; then
  echo "gc reported active image dir as candidate" >&2
  exit 1
fi
echo "pass: gc dry-run reports image staging/orphan candidates only"

"$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  delete "$vm_name" >/dev/null
vm_exists=0

removed_json="$("$kumabox_path" --root-dir "$root_dir" image rm "$image_name")"
printf '%s\n' "$removed_json"
if [[ "$(printf '%s' "$removed_json" | jq -r '.id')" != "$image_id" ]]; then
  echo "image rm removed unexpected image" >&2
  exit 1
fi
if [[ -e "$image_dir" ]]; then
  echo "image dir still exists after image rm: $image_dir" >&2
  exit 1
fi
if "$kumabox_path" --root-dir "$root_dir" image inspect "$image_name" --json >/tmp/kumabox-image-inspect.out 2>&1; then
  echo "image inspect unexpectedly succeeded after image rm" >&2
  exit 1
fi

echo "P1-06/P1-07 image rm and gc verification passed"
