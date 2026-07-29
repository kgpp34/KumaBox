#!/usr/bin/env bash
set -Eeuo pipefail

# Single entry point for the Linux KumaBox end-to-end validation suite.
# Individual suites remain reusable through verify.sh, while this command
# owns the common paths and failure diagnostics users need during bring-up.

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd -- "$script_dir/../.." && pwd)

kumabox=./bin/kumabox
cloud_hypervisor=cloud-hypervisor
qemu_img=qemu-img
root_dir=/tmp/kumabox-p0/data
run_dir=/tmp/kumabox-p0/run
log_dir=/tmp/kumabox-p0/logs
image=p6-agent-image
image_ref=kumabox/ubuntu:24.04-p6
network=cni:default
storage=64M
metadata_backend=json
metadata_path=
use_sudo=false
skip_unit=false
keep_failed=true
skip_hotplug=false
rebuild_image=false

usage() {
  cat <<'EOF'
Usage: scripts/linux/e2e.sh [options]

Runs the complete Linux validation sequence in order:
  unit -> OCI boot/exec/disk -> CNI datapath -> snapshot/restore/clone -> hotplug

Defaults match the standard KumaBox validation layout:
  root: /tmp/kumabox-p0/data
  run:  /tmp/kumabox-p0/run
  logs: /tmp/kumabox-p0/logs

Options:
  --kumabox PATH
  --cloud-hypervisor PATH
  --qemu-img PATH
  --root-dir PATH
  --run-dir PATH
  --log-dir PATH
  --image NAME
  --image-ref REF             OCI ref used with --rebuild-image
  --network NETWORK
  --storage SIZE
  --metadata-backend json|sqlite
  --metadata-path PATH       SQLite path; defaults to ROOT/metadata/kumabox.db
  --skip-unit
  --sudo
  --cleanup-on-success
  --skip-hotplug
  --rebuild-image             rebuild the managed OCI image before boot suites
EOF
}

require_value() {
  [[ -n ${2:-} ]] || { echo "$1 requires a value" >&2; exit 2; }
}

while (($#)); do
  case "$1" in
    --kumabox) require_value "$1" "${2:-}"; kumabox=$2; shift 2 ;;
    --cloud-hypervisor) require_value "$1" "${2:-}"; cloud_hypervisor=$2; shift 2 ;;
    --qemu-img) require_value "$1" "${2:-}"; qemu_img=$2; shift 2 ;;
    --root-dir) require_value "$1" "${2:-}"; root_dir=$2; shift 2 ;;
    --run-dir) require_value "$1" "${2:-}"; run_dir=$2; shift 2 ;;
    --log-dir) require_value "$1" "${2:-}"; log_dir=$2; shift 2 ;;
    --image) require_value "$1" "${2:-}"; image=$2; shift 2 ;;
    --image-ref) require_value "$1" "${2:-}"; image_ref=$2; shift 2 ;;
    --network) require_value "$1" "${2:-}"; network=$2; shift 2 ;;
    --storage) require_value "$1" "${2:-}"; storage=$2; shift 2 ;;
    --metadata-backend) require_value "$1" "${2:-}"; metadata_backend=$2; shift 2 ;;
    --metadata-path) require_value "$1" "${2:-}"; metadata_path=$2; shift 2 ;;
    --skip-unit) skip_unit=true; shift ;;
    --sudo) use_sudo=true; shift ;;
    --cleanup-on-success) keep_failed=false; shift ;;
    --skip-hotplug) skip_hotplug=true; shift ;;
    --rebuild-image) rebuild_image=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ $(uname -s) == Linux ]] || { echo "Linux is required" >&2; exit 1; }
[[ "$metadata_backend" == json || "$metadata_backend" == sqlite ]] || { echo "--metadata-backend must be json or sqlite" >&2; exit 2; }
[[ -x "$kumabox" ]] || { echo "kumabox is not executable: $kumabox" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "required command not found: jq" >&2; exit 1; }
command -v "$cloud_hypervisor" >/dev/null 2>&1 || { echo "required command not found: $cloud_hypervisor" >&2; exit 1; }
command -v "$qemu_img" >/dev/null 2>&1 || { echo "required command not found: $qemu_img" >&2; exit 1; }

if [[ "$use_sudo" == true ]]; then
  command -v sudo >/dev/null 2>&1 || { echo "sudo is required by --sudo" >&2; exit 1; }
  sudo -v
  prefix=(sudo)
else
  prefix=()
fi

common_args=(
  --kumabox "$kumabox"
  --cloud-hypervisor "$cloud_hypervisor"
  --qemu-img "$qemu_img"
  --root-dir "$root_dir"
  --run-dir "$run_dir"
  --log-dir "$log_dir"
  --storage "$storage"
  --metadata-backend "$metadata_backend"
)
if [[ -n "$metadata_path" ]]; then
  common_args+=(--metadata-path "$metadata_path")
fi

print_failure_context() {
  local status=$?
  [[ $status -eq 0 ]] && return
  printf '\n==> E2E failure context\n' >&2
  printf 'root_dir=%s\nrun_dir=%s\nlog_dir=%s\n' "$root_dir" "$run_dir" "$log_dir" >&2
  if [[ -d "$run_dir/vms" ]]; then
    find "$run_dir/vms" -maxdepth 2 -type f \( -name console.log -o -name cloud-hypervisor.stderr.log -o -name cloud-hypervisor.json \) -print >&2 || true
  fi
  if [[ -d "$log_dir/vms" ]]; then
    find "$log_dir/vms" -maxdepth 2 -type f -print >&2 || true
  fi
  printf 'failed state is preserved for inspection.\n' >&2
}
trap print_failure_context EXIT

run_suite() {
  local suite=$1
  printf '\n==> E2E suite: %s\n' "$suite"
  case "$suite" in
    oci)
      suite_args=("${common_args[@]}" --image-name "$image" --image-ref "$image_ref" --skip-base-build --sudo)
      if [[ "$rebuild_image" != true ]]; then
        suite_args+=(--skip-image-build)
      fi
      "${prefix[@]}" "$script_dir/verify.sh" "$suite" "${suite_args[@]}"
      ;;
    boot)
      suite_args=(
        --kumabox "$kumabox"
        --cloud-hypervisor "$cloud_hypervisor"
        --qemu-img "$qemu_img"
        --root-dir "$root_dir"
        --run-dir "$run_dir"
        --log-dir "$log_dir"
        --image-name "$image"
        --image-ref "$image_ref"
        --network "$network"
        --storage "$storage"
        --metadata-backend "$metadata_backend"
        --skip-build
        --skip-image-build
      )
      if [[ -n "$metadata_path" ]]; then
        suite_args+=(--metadata-path "$metadata_path")
      fi
      "${prefix[@]}" "$script_dir/verify-oci-boot-disk.sh" "${suite_args[@]}"
      ;;
    cni|snapshot)
      "${prefix[@]}" "$script_dir/verify.sh" "$suite" "${common_args[@]}" --image "$image" --sudo
      ;;
    hotplug)
      suite_args=(
        --kumabox "$kumabox"
        --cloud-hypervisor "$cloud_hypervisor"
        --qemu-img "$qemu_img"
        --root-dir "$root_dir"
        --run-dir "$run_dir"
        --log-dir "$log_dir"
        --image "$image"
        --network "$network"
        --storage "$storage"
        --metadata-backend "$metadata_backend"
        --sudo
      )
      if [[ -n "$metadata_path" ]]; then
        suite_args+=(--metadata-path "$metadata_path")
      fi
      "${prefix[@]}" "$script_dir/verify-p8-hotplug.sh" "${suite_args[@]}"
      ;;
    *)
      echo "unknown E2E suite: $suite" >&2
      return 2
      ;;
  esac
}

if [[ "$skip_unit" != true ]]; then
  printf '\n==> E2E suite: unit\n'
  if [[ "$(id -u)" -eq 0 ]]; then
    [[ -n "${SUDO_USER:-}" && "$SUDO_USER" != root ]] || {
      echo 'cannot run unit tests as root: invoke with sudo from the development user' >&2
      exit 1
    }
    sudo -iu "$SUDO_USER" bash -lc "cd '$repo_root' && GOTOOLCHAIN=local go test ./... -count=1"
  else
    (cd "$repo_root" && GOTOOLCHAIN=local go test ./... -count=1)
  fi
fi

run_suite oci
run_suite boot
run_suite cni
run_suite snapshot
if [[ "$skip_hotplug" != true ]]; then
  run_suite hotplug
fi

if [[ "$keep_failed" == false ]]; then
  printf '\n==> cleanup after successful E2E\n'
  "${prefix[@]}" "$script_dir/reset-p6-environment.sh" --yes
fi

printf '\nPASS: KumaBox Linux E2E completed\n'
