#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
cloud_hypervisor_path="cloud-hypervisor"
qemu_img_path="qemu-img"
root_dir="/tmp/kumabox-p0/data"
run_dir="/tmp/kumabox-p0/run"
log_dir="/tmp/kumabox-p0/logs"
ref="kumabox/ubuntu:24.04-p3"
platform="linux/amd64"
image_name="p3-agent-image"
vm_name="p3-agent"
storage_size="64M"
source="auto"
mkfs_erofs="mkfs.erofs"
timeout="90s"
skip_base_build=0
use_sudo=false
script_status=1

usage() {
  cat <<'USAGE'
Usage: scripts/linux/p3/verify-oci-agent-transport.sh [options]

Options:
  --kumabox PATH             kumabox binary path, defaults to ./bin/kumabox
  --cloud-hypervisor PATH    cloud-hypervisor path, defaults to cloud-hypervisor
  --qemu-img PATH            qemu-img path, defaults to qemu-img
  --root-dir PATH            state root directory, defaults to /tmp/kumabox-p0/data
  --run-dir PATH             runtime directory, defaults to /tmp/kumabox-p0/run
  --log-dir PATH             log directory, defaults to /tmp/kumabox-p0/logs
  --ref REF                  OCI image ref, defaults to kumabox/ubuntu:24.04-p3
  --platform VALUE           OCI platform, defaults to linux/amd64
  --image-name NAME          built image name, defaults to p3-agent-image
  --name NAME                VM name, defaults to p3-agent
  --storage SIZE             per-VM COW size, defaults to 64M
  --source VALUE             OCI source: auto, registry, or daemon. Defaults to auto
  --mkfs-erofs PATH          mkfs.erofs binary path, defaults to mkfs.erofs
  --timeout DURATION         agent ping timeout, defaults to 90s
  --skip-base-build          use existing local OCI base image
  --sudo                     run kumabox and root-owned file reads through sudo

Verifies minimal OCI guest-agent transport:
build base image with kumabox-agent -> build OCI image -> run VM with vsock ->
kumabox agent ping returns guest hello -> delete VM and image record.
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

step() {
  printf '\n==> %s\n' "$1"
}

print_failure_context() {
  set +e
  step "failure context: VM inspect"
  kb inspect "$vm_name" --json 2>/dev/null || true

  if [[ -n "${config_path:-}" && "$config_path" != "null" ]]; then
    step "failure context: rendered config"
    "${cat_cmd[@]}" "$config_path" 2>/dev/null | jq '.' || true
  fi

  if [[ -n "${run_json:-}" ]]; then
    vm_log_dir="$(printf '%s' "$run_json" | jq -r '.logDir // empty' 2>/dev/null)"
    if [[ -n "$vm_log_dir" && "$vm_log_dir" != "null" ]]; then
      step "failure context: console tail"
      "${cat_cmd[@]}" "$vm_log_dir/console.log" 2>/dev/null | tail -n 200 || true

      step "failure context: cloud-hypervisor stderr"
      "${cat_cmd[@]}" "$vm_log_dir/cloud-hypervisor.stderr.log" 2>/dev/null | tail -n 120 || true

      step "failure context: cloud-hypervisor stdout"
      "${cat_cmd[@]}" "$vm_log_dir/cloud-hypervisor.stdout.log" 2>/dev/null | tail -n 120 || true
    fi
  fi
}

on_exit() {
  if [[ "$script_status" -ne 0 ]]; then
    print_failure_context
    printf '\n==> preserving failed OCI agent state\n' >&2
    printf 'state: preserved root_dir=%s run_dir=%s log_dir=%s\n' "$root_dir" "$run_dir" "$log_dir" >&2
  fi
}
trap on_exit EXIT

while [[ $# -gt 0 ]]; do
  case "$1" in
    --kumabox) require_value "$1" "${2:-}"; kumabox_path="$2"; shift 2 ;;
    --cloud-hypervisor) require_value "$1" "${2:-}"; cloud_hypervisor_path="$2"; shift 2 ;;
    --qemu-img) require_value "$1" "${2:-}"; qemu_img_path="$2"; shift 2 ;;
    --root-dir) require_value "$1" "${2:-}"; root_dir="$2"; shift 2 ;;
    --run-dir) require_value "$1" "${2:-}"; run_dir="$2"; shift 2 ;;
    --log-dir) require_value "$1" "${2:-}"; log_dir="$2"; shift 2 ;;
    --ref) require_value "$1" "${2:-}"; ref="$2"; shift 2 ;;
    --platform) require_value "$1" "${2:-}"; platform="$2"; shift 2 ;;
    --image-name) require_value "$1" "${2:-}"; image_name="$2"; shift 2 ;;
    --name) require_value "$1" "${2:-}"; vm_name="$2"; shift 2 ;;
    --storage) require_value "$1" "${2:-}"; storage_size="$2"; shift 2 ;;
    --source) require_value "$1" "${2:-}"; source="$2"; shift 2 ;;
    --mkfs-erofs) require_value "$1" "${2:-}"; mkfs_erofs="$2"; shift 2 ;;
    --timeout) require_value "$1" "${2:-}"; timeout="$2"; shift 2 ;;
    --skip-base-build) skip_base_build=1; shift ;;
    --sudo) use_sudo=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [[ ! -x "$kumabox_path" ]]; then
  echo "kumabox is not executable: $kumabox_path" >&2
  exit 1
fi
for bin in jq mkfs.ext4; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "$bin is required for OCI agent transport verification" >&2
    exit 1
  fi
done
if [[ "$mkfs_erofs" == */* ]]; then
  if [[ ! -x "$mkfs_erofs" ]]; then
    echo "mkfs.erofs is not executable: $mkfs_erofs" >&2
    exit 1
  fi
elif ! command -v "$mkfs_erofs" >/dev/null 2>&1; then
  echo "mkfs.erofs is required for OCI agent transport verification" >&2
  exit 1
fi

if [[ "$use_sudo" == true ]]; then
  if ! command -v sudo >/dev/null 2>&1; then
    echo "--sudo requested but sudo is missing" >&2
    exit 1
  fi
  kumabox_cmd=(sudo "$kumabox_path")
  cat_cmd=(sudo cat)
  remove_cmd=(sudo rm -rf)
  mkdir_cmd=(sudo mkdir -p)
else
  kumabox_cmd=("$kumabox_path")
  cat_cmd=(cat)
  remove_cmd=(rm -rf)
  mkdir_cmd=(mkdir -p)
fi

kb() {
  "${kumabox_cmd[@]}" \
    --root-dir "$root_dir" \
    --run-dir "$run_dir" \
    --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor_path" \
    "$@"
}

cleanup() {
  kb delete "$vm_name" --force >/dev/null 2>&1 || true
  kb image rm "$image_name" >/dev/null 2>&1 || true
}

if [[ "$skip_base_build" -eq 0 ]]; then
  step "build OCI base image with guest agent"
  scripts/linux/verify.sh p3 base-image --tag "$ref" --platform "$platform" --keep-fixture
fi

step "clean previous OCI agent state"
cleanup
"${remove_cmd[@]}" "$root_dir" "$run_dir" "$log_dir"
"${mkdir_cmd[@]}" "$root_dir" "$run_dir" "$log_dir"

step "environment checks"
scripts/linux/env-check.sh \
  --kumabox "$kumabox_path" \
  --cloud-hypervisor "$cloud_hypervisor_path" \
  --qemu-img "$qemu_img_path" \
  --strict

step "build OCI image"
image_json="$(kb image build "$ref" \
  --name "$image_name" \
  --platform "$platform" \
  --source "$source" \
  --mkfs-erofs "$mkfs_erofs" \
  --json)"
printf '%s\n' "$image_json"

step "run OCI VM with vsock"
run_json="$(kb run "$image_name" \
  --name "$vm_name" \
  --storage "$storage_size" \
  --network none)"
printf '%s\n' "$run_json"

state="$(printf '%s' "$run_json" | jq -r '.state')"
config_path="$(printf '%s' "$run_json" | jq -r '.config')"
vsock_socket="$(printf '%s' "$run_json" | jq -r '.vsockSocket')"
if [[ "$state" != "running" ]]; then
  echo "VM did not reach running state: $state" >&2
  exit 1
fi
if [[ -z "$vsock_socket" || "$vsock_socket" == "null" ]]; then
  echo "VM record is missing vsockSocket" >&2
  exit 1
fi
printf 'state: vsockSocket=%s config=%s\n' "$vsock_socket" "$config_path"

step "inspect rendered vsock config"
config_json="$("${cat_cmd[@]}" "$config_path")"
printf '%s\n' "$config_json" | jq '.kernel, .vsock'
cmdline="$(printf '%s\n' "$config_json" | jq -r '.kernel.cmdline')"
if [[ "$cmdline" != *"boot=kumabox-overlay"* || "$cmdline" == *"root=/dev/ram0"* ]]; then
  echo "cmdline does not select KumaBox overlay boot: $cmdline" >&2
  exit 1
fi
rendered_socket="$(printf '%s\n' "$config_json" | jq -r '.vsock.socket')"
if [[ "$rendered_socket" != "$vsock_socket" ]]; then
  echo "rendered vsock socket mismatch: got $rendered_socket want $vsock_socket" >&2
  exit 1
fi

step "ping guest agent"
set +e
agent_output="$(kb agent ping "$vm_name" --timeout "$timeout" 2>&1)"
agent_status=$?
set -e
if [[ "$agent_status" -ne 0 ]]; then
  printf '%s\n' "$agent_output" >&2
  exit "$agent_status"
fi
agent_json="$agent_output"
printf '%s\n' "$agent_json"
agent_ok="$(printf '%s' "$agent_json" | jq -r '.agent.ok')"
agent_os="$(printf '%s' "$agent_json" | jq -r '.agent.os')"
if [[ "$agent_ok" != "true" || "$agent_os" != "linux" ]]; then
  echo "agent ping failed readiness checks" >&2
  exit 1
fi

step "delete VM and cleanup image"
delete_json="$(kb delete "$vm_name" --force)"
printf '%s\n' "$delete_json"
kb image rm "$image_name" >/dev/null

script_status=0
echo "P3-08 OCI agent transport verification passed"
