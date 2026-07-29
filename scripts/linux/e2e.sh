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
root_dir=/var/lib/kumabox
run_dir=/run/kumabox
log_dir=/var/log/kumabox
image=p6-agent-image
image_ref=kumabox/ubuntu:24.04-p6
network=cni:default
storage=64M
metadata_backend=json
use_sudo=false
skip_unit=false
keep_failed=true
skip_hotplug=false
rebuild_image=false
go_bin_override=

usage() {
  cat <<'EOF'
Usage: scripts/linux/e2e.sh [options]

Runs the complete Linux validation sequence in order:
  unit -> OCI boot/exec/disk -> CNI datapath -> snapshot/restore/clone -> hotplug

Defaults match the standard KumaBox validation layout:
  root: /var/lib/kumabox
  run:  /run/kumabox
  logs: /var/log/kumabox

Options:
  --kumabox PATH
  --cloud-hypervisor PATH
  --qemu-img PATH
  --image NAME
  --image-ref REF             OCI ref used with --rebuild-image
  --network NETWORK
  --storage SIZE
  --metadata-backend json|sqlite
  --skip-unit
  --sudo
  --cleanup-on-success
  --skip-hotplug
  --rebuild-image             rebuild the managed OCI image before boot suites
  --go-bin PATH               Go 1.24.4+ binary from the development user
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
    --root-dir|--run-dir|--log-dir|--metadata-path)
      echo "$1 is not configurable for the E2E suite; use KumaBox system defaults" >&2
      exit 2
      ;;
    --image) require_value "$1" "${2:-}"; image=$2; shift 2 ;;
    --image-ref) require_value "$1" "${2:-}"; image_ref=$2; shift 2 ;;
    --network) require_value "$1" "${2:-}"; network=$2; shift 2 ;;
    --storage) require_value "$1" "${2:-}"; storage=$2; shift 2 ;;
    --metadata-backend) require_value "$1" "${2:-}"; metadata_backend=$2; shift 2 ;;
    --skip-unit) skip_unit=true; shift ;;
    --sudo) use_sudo=true; shift ;;
    --cleanup-on-success) keep_failed=false; shift ;;
    --skip-hotplug) skip_hotplug=true; shift ;;
    --rebuild-image) rebuild_image=true; shift ;;
    --go-bin) require_value "$1" "${2:-}"; go_bin_override=$2; shift 2 ;;
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
  --storage "$storage"
  --metadata-backend "$metadata_backend"
)

print_failure_context() {
  local status=$?
  [[ $status -eq 0 ]] && return
  printf '\n==> E2E failure context\n' >&2
  printf 'root_dir=/var/lib/kumabox\nrun_dir=/run/kumabox\nlog_dir=/var/log/kumabox\n' >&2
  if [[ -d "$run_dir/vms" ]]; then
    find "$run_dir/vms" -maxdepth 2 -type f \( -name console.log -o -name cloud-hypervisor.stderr.log -o -name cloud-hypervisor.json \) -print >&2 || true
  fi
  if [[ -d "$log_dir/vms" ]]; then
    find "$log_dir/vms" -maxdepth 2 -type f -print >&2 || true
  fi
  printf 'failed state is preserved for inspection.\n' >&2
}

user_go_path() {
  local user="$1"
  local shell_path candidate version_command version_output alternate_shell
  shell_path="$(getent passwd "$user" | cut -d: -f7)"
  [[ -x "$shell_path" ]] || shell_path=/bin/bash
  if [[ -n "$go_bin_override" ]]; then
    printf -v version_command '%q version' "$go_bin_override"
    version_output="$(sudo -u "$user" -H "$shell_path" -ic "$version_command" 2>/dev/null || true)"
    if printf '%s\n' "$version_output" | grep -Eq 'go1\.24\.([4-9]|[1-9][0-9])([[:space:]]|$)|go1\.(2[5-9]|[3-9][0-9])([.[:space:]]|$)'; then
      printf '%s\n' "$go_bin_override"
      return 0
    fi
    return 1
  fi
  while IFS= read -r candidate; do
    printf -v version_command '%q version' "$candidate"
    version_output="$(sudo -u "$user" -H "$shell_path" -ic "$version_command" 2>/dev/null || true)"
    if printf '%s\n' "$version_output" | grep -Eq 'go1\.24\.([4-9]|[1-9][0-9])([[:space:]]|$)|go1\.(2[5-9]|[3-9][0-9])([.[:space:]]|$)'; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done < <(
    {
      sudo -u "$user" -H "$shell_path" -ic 'command -v go1.24.4; command -v go1.24; command -v go' 2>/dev/null || true
      sudo -u "$user" -H "$shell_path" -lic 'command -v go1.24.4; command -v go1.24; command -v go' 2>/dev/null || true
      for alternate_shell in /bin/bash /bin/zsh; do
        [[ -x "$alternate_shell" && "$alternate_shell" != "$shell_path" ]] || continue
        sudo -u "$user" -H "$alternate_shell" -ic 'command -v go1.24.4; command -v go1.24; command -v go' 2>/dev/null || true
        sudo -u "$user" -H "$alternate_shell" -lic 'command -v go1.24.4; command -v go1.24; command -v go' 2>/dev/null || true
      done
    } | awk 'NF && !seen[$0]++'
  )
  return 1
}
trap print_failure_context EXIT

run_suite() {
  local suite=$1
  printf '\n==> E2E suite: %s\n' "$suite"
  case "$suite" in
    oci)
      suite_args=("${common_args[@]}" --image-name "$image" --ref "$image_ref" --sudo)
      [[ -n "$go_bin_override" ]] && suite_args+=(--go-bin "$go_bin_override")
      if [[ "$rebuild_image" != true ]]; then
        suite_args+=(--skip-base-build --skip-image-build)
      fi
      "${prefix[@]}" "$script_dir/verify.sh" "$suite" "${suite_args[@]}"
      ;;
    boot)
      suite_args=(
        --kumabox "$kumabox"
        --cloud-hypervisor "$cloud_hypervisor"
        --qemu-img "$qemu_img"
        --image-name "$image"
        --image-ref "$image_ref"
        --network "$network"
        --storage "$storage"
        --metadata-backend "$metadata_backend"
        --skip-build
        --skip-image-build
      )
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
        --image "$image"
        --network "$network"
        --storage "$storage"
        --metadata-backend "$metadata_backend"
        --sudo
      )
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
    build_go="$(user_go_path "$SUDO_USER")" || {
      echo "cannot find Go 1.24.4 or newer in $SUDO_USER login environment" >&2
      exit 1
    }
    build_path="$(dirname "$build_go"):${PATH:-/usr/bin:/bin}"
    sudo -u "$SUDO_USER" -H env PATH="$build_path" bash -lc "cd '$repo_root' && GOTOOLCHAIN=local '$build_go' test ./... -count=1"
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
