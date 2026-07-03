#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
root_dir="/tmp/kumabox-p0/data"
run_dir="/tmp/kumabox-p0/run"
log_dir="/tmp/kumabox-p0/logs"
cloud_hypervisor_path="cloud-hypervisor"
qemu_img_path="qemu-img"
image_name="p1-image-ref"
create_name="p1-image-create"
run_name="p1-image-run"
root_disk=""
firmware=""

usage() {
  cat <<'USAGE'
Usage: scripts/linux/verify-image-ref.sh --root-disk PATH --firmware PATH [options]

Options:
  --kumabox PATH             kumabox binary path, defaults to ./bin/kumabox
  --cloud-hypervisor PATH    cloud-hypervisor binary path, defaults to cloud-hypervisor
  --qemu-img PATH            qemu-img binary path, defaults to qemu-img
  --root-dir PATH            store directory, defaults to /tmp/kumabox-p0/data
  --run-dir PATH             runtime directory, defaults to /tmp/kumabox-p0/run
  --log-dir PATH             log directory, defaults to /tmp/kumabox-p0/logs
  --image-name NAME          image name, defaults to p1-image-ref
  --create-name NAME         create VM name, defaults to p1-image-create
  --run-name NAME            run VM name, defaults to p1-image-run
  --root-disk PATH           cloud image root disk path
  --firmware PATH            UEFI firmware path for cloud-image boot

Verifies P1-05 image references:
clean data/run/logs -> image import -> create IMAGE -> delete VM -> run IMAGE
-> inspect image ref/root disk -> delete VM -> clean data/run/logs.
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
    --create-name)
      require_value "$1" "${2:-}"
      create_name="$2"
      shift 2
      ;;
    --run-name)
      require_value "$1" "${2:-}"
      run_name="$2"
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
  echo "verify-image-ref must run inside the Linux VM" >&2
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

created_vm_exists=0
run_vm_exists=0
cleanup() {
  set +e
  if [[ "$created_vm_exists" -eq 1 ]]; then
    "$kumabox_path" --root-dir "$root_dir" --run-dir "$run_dir" --log-dir "$log_dir" --cloud-hypervisor-bin "$cloud_hypervisor_path" delete "$create_name" --force >/dev/null 2>&1
  fi
  if [[ "$run_vm_exists" -eq 1 ]]; then
    "$kumabox_path" --root-dir "$root_dir" --run-dir "$run_dir" --log-dir "$log_dir" --cloud-hypervisor-bin "$cloud_hypervisor_path" delete "$run_name" --force >/dev/null 2>&1
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
if [[ -z "$image_id" || "$image_id" == "null" ]]; then
  echo "could not parse image id from import output" >&2
  exit 1
fi
if [[ -z "$image_root_disk" || "$image_root_disk" == "null" || ! -f "$image_root_disk" ]]; then
  echo "managed image root disk is missing: $image_root_disk" >&2
  exit 1
fi

created_json="$("$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  create "$image_name" \
  --name "$create_name")"
created_vm_exists=1
printf '%s\n' "$created_json"

created_config="$(printf '%s' "$created_json" | jq -r '.config')"
if [[ "$(printf '%s' "$created_json" | jq -r '.image.id')" != "$image_id" ]]; then
  echo "created VM did not record image id" >&2
  exit 1
fi
if [[ "$(printf '%s' "$created_json" | jq -r '.rootDisk')" != "$image_root_disk" ]]; then
  echo "created VM root disk is not the managed image root disk" >&2
  exit 1
fi
if [[ "$(jq -r '.disks[0].path' "$created_config")" != "$image_root_disk" ]]; then
  echo "rendered config does not use managed image root disk" >&2
  jq '.disks' "$created_config" >&2
  exit 1
fi
echo "pass: create IMAGE records image ref and root disk"

"$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  delete "$create_name" >/dev/null
created_vm_exists=0

if [[ ! -f "$image_root_disk" ]]; then
  echo "image root disk was deleted with VM: $image_root_disk" >&2
  exit 1
fi
echo "pass: delete VM preserves image root disk"

run_json="$("$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  run "$image_name" \
  --name "$run_name")"
run_vm_exists=1
printf '%s\n' "$run_json"

if [[ "$(printf '%s' "$run_json" | jq -r '.state')" != "running" ]]; then
  echo "run IMAGE did not enter running state" >&2
  exit 1
fi
if [[ "$(printf '%s' "$run_json" | jq -r '.image.id')" != "$image_id" ]]; then
  echo "run VM did not record image id" >&2
  exit 1
fi
if [[ "$(printf '%s' "$run_json" | jq -r '.rootDisk')" != "$image_root_disk" ]]; then
  echo "run VM root disk is not the managed image root disk" >&2
  exit 1
fi
echo "pass: run IMAGE starts VM with managed image root disk"

"$kumabox_path" \
  --root-dir "$root_dir" \
  --run-dir "$run_dir" \
  --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor_path" \
  delete "$run_name" --force >/dev/null
run_vm_exists=0

if [[ ! -f "$image_root_disk" ]]; then
  echo "image root disk was deleted after run VM cleanup: $image_root_disk" >&2
  exit 1
fi

echo "P1-05 image ref verification passed"
