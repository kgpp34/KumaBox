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
image_name="p3-agent-image-v3"
vm_name="p3-exec"
storage_size="64M"
source="auto"
mkfs_erofs="mkfs.erofs"
timeout="120s"
skip_base_build=0
skip_image_build=0
use_sudo=false
metadata_backend=json
metadata_path=
script_status=1
tail_pid=""

usage() {
  cat <<'USAGE'
Usage: scripts/linux/verify-oci.sh [options]

Options:
  --kumabox PATH             kumabox binary path, defaults to ./bin/kumabox
  --cloud-hypervisor PATH    cloud-hypervisor path, defaults to cloud-hypervisor
  --qemu-img PATH            qemu-img path, defaults to qemu-img
  --root-dir PATH            state root directory, defaults to /tmp/kumabox-p0/data
  --run-dir PATH             runtime directory, defaults to /tmp/kumabox-p0/run
  --log-dir PATH             log directory, defaults to /tmp/kumabox-p0/logs
  --ref REF                  OCI image ref, defaults to kumabox/ubuntu:24.04-p3
  --platform VALUE           OCI platform, defaults to linux/amd64
  --image-name NAME          managed image name, defaults to p3-agent-image-v3
  --name NAME                VM name, defaults to p3-exec
  --storage SIZE             per-VM COW size, defaults to 64M
  --source VALUE             OCI source: auto, registry, or daemon. Defaults to auto
  --mkfs-erofs PATH          mkfs.erofs binary path, defaults to mkfs.erofs
  --timeout DURATION         agent timeout, defaults to 120s
  --skip-base-build          use existing local OCI base image
  --skip-image-build         use an existing managed image without rebuilding it
  --sudo                     run kumabox and root-owned file reads through sudo
  --metadata-backend VALUE   metadata backend: json or sqlite
  --metadata-path PATH       SQLite metadata path

Verifies OCI exec MVP:
build OCI image -> run VM -> wait for agent -> execute hostname/stdin/env/failing
commands through kumabox exec -> delete VM while preserving the managed image
for the CNI and snapshot E2E suites.
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

now_ms() {
  date +%s%3N
}

stop_console_tail() {
  if [[ -n "$tail_pid" ]]; then
    kill "$tail_pid" >/dev/null 2>&1 || true
    wait "$tail_pid" >/dev/null 2>&1 || true
    tail_pid=""
  fi
}

print_failure_context() {
  set +e
  step "failure context: VM inspect"
  kb inspect "$vm_name" --json 2>/dev/null || true

  if [[ -n "${config_path:-}" && "$config_path" != "null" ]]; then
    step "failure context: rendered config"
    "${cat_cmd[@]}" "$config_path" 2>/dev/null | jq '.' || true
  fi

  if [[ -n "${vm_log_dir:-}" && "$vm_log_dir" != "null" ]]; then
    step "failure context: console tail"
    "${cat_cmd[@]}" "$vm_log_dir/console.log" 2>/dev/null | tail -n 240 || true

    step "failure context: cloud-hypervisor stderr"
    "${cat_cmd[@]}" "$vm_log_dir/cloud-hypervisor.stderr.log" 2>/dev/null | tail -n 120 || true
  fi
}

on_exit() {
  stop_console_tail
  if [[ "$script_status" -ne 0 ]]; then
    print_failure_context
    printf '\n==> preserving failed OCI exec state\n' >&2
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
    --skip-image-build) skip_image_build=1; shift ;;
    --sudo) use_sudo=true; shift ;;
    --metadata-backend) require_value "$1" "${2:-}"; metadata_backend="$2"; shift 2 ;;
    --metadata-path) require_value "$1" "${2:-}"; metadata_path="$2"; shift 2 ;;
    -h|--help) script_status=0; usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ "$metadata_backend" == json || "$metadata_backend" == sqlite ]] || { echo "--metadata-backend must be json or sqlite" >&2; exit 2; }

if [[ ! -x "$kumabox_path" ]]; then
  echo "kumabox is not executable: $kumabox_path" >&2
  exit 1
fi
for bin in jq mkfs.ext4; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "$bin is required for OCI exec verification" >&2
    exit 1
  fi
done
if [[ "$mkfs_erofs" == */* ]]; then
  if [[ ! -x "$mkfs_erofs" ]]; then
    echo "mkfs.erofs is not executable: $mkfs_erofs" >&2
    exit 1
  fi
elif ! command -v "$mkfs_erofs" >/dev/null 2>&1; then
  echo "mkfs.erofs is required for OCI exec verification" >&2
  exit 1
fi

if [[ "$use_sudo" == true ]]; then
  if ! command -v sudo >/dev/null 2>&1; then
    echo "--sudo requested but sudo is missing" >&2
    exit 1
  fi
  kumabox_cmd=(sudo "$kumabox_path")
  cat_cmd=(sudo cat)
  tail_cmd=(sudo tail)
  remove_cmd=(sudo rm -rf)
  mkdir_cmd=(sudo mkdir -p)
else
  kumabox_cmd=("$kumabox_path")
  cat_cmd=(cat)
  tail_cmd=(tail)
  remove_cmd=(rm -rf)
  mkdir_cmd=(mkdir -p)
fi

kb() {
  local metadata_args=(--metadata-backend "$metadata_backend")
  if [[ -n "$metadata_path" ]]; then
    metadata_args+=(--metadata-path "$metadata_path")
  fi
  "${kumabox_cmd[@]}" \
    --root-dir "$root_dir" \
    --run-dir "$run_dir" \
    --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor_path" \
    "${metadata_args[@]}" \
    "$@"
}

cleanup() {
  kb delete "$vm_name" --force >/dev/null 2>&1 || true
  if [[ "$skip_image_build" -eq 0 ]]; then
    kb image rm "$image_name" >/dev/null 2>&1 || true
  fi
}

build_base_image() {
  local context_dir=oci-images/ubuntu
  local dockerfile=$context_dir/24.04/Dockerfile
  local agent_binary=$context_dir/kumabox-agent-linux-amd64

  for binary in docker go; do
    command -v "$binary" >/dev/null 2>&1 || {
      echo "$binary is required to build the KumaBox OCI base image" >&2
      return 1
    }
  done
  [[ -f $dockerfile ]] || { echo "OCI base Dockerfile is missing: $dockerfile" >&2; return 1; }

  step "build Linux guest agent"
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$agent_binary" ./cmd/agent

  step "build KumaBox-compatible OCI base image"
  if ! docker build --platform "$platform" -f "$dockerfile" -t "$ref" "$context_dir"; then
    rm -f "$agent_binary"
    return 1
  fi
  rm -f "$agent_binary"
}

if [[ "$skip_base_build" -eq 0 ]]; then
  build_base_image
fi

step "clean previous OCI exec state"
cleanup
"${remove_cmd[@]}" "$root_dir" "$run_dir" "$log_dir"
"${mkdir_cmd[@]}" "$root_dir" "$run_dir" "$log_dir"

step "environment checks"
scripts/linux/env-check.sh \
  --kumabox "$kumabox_path" \
  --cloud-hypervisor "$cloud_hypervisor_path" \
  --qemu-img "$qemu_img_path" \
  --strict

if [[ "$skip_image_build" -eq 1 ]]; then
  step "use existing managed OCI image"
  kb image inspect "$image_name" --json >/dev/null || {
    echo "managed image not found: $image_name" >&2
    exit 1
  }
else
  step "build OCI image"
  image_json="$(kb image build "$ref" \
    --name "$image_name" \
    --platform "$platform" \
    --source "$source" \
    --mkfs-erofs "$mkfs_erofs" \
    --json)"
  printf '%s\n' "$image_json"
fi

step "run OCI VM"
run_start_ms="$(now_ms)"
run_json="$(kb run "$image_name" \
  --name "$vm_name" \
  --storage "$storage_size" \
  --network none)"
run_done_ms="$(now_ms)"
printf '%s\n' "$run_json"

state="$(printf '%s' "$run_json" | jq -r '.state')"
config_path="$(printf '%s' "$run_json" | jq -r '.config')"
vm_log_dir="$(printf '%s' "$run_json" | jq -r '.logDir')"
console_log="$vm_log_dir/console.log"
if [[ "$state" != "running" ]]; then
  echo "VM did not reach running state: $state" >&2
  exit 1
fi

step "tail console while waiting for agent"
printf 'state: console=%s\n' "$console_log"
"${tail_cmd[@]}" -n +1 -F "$console_log" &
tail_pid=$!

step "wait for guest agent"
agent_start_ms="$(now_ms)"
agent_json="$(kb agent ping "$vm_name" --timeout "$timeout")"
printf '%s\n' "$agent_json"
agent_done_ms="$(now_ms)"
stop_console_tail

step "exec hostname"
hostname_out="$(kb exec "$vm_name" -- uname -n)"
printf 'state: hostname=%s\n' "$hostname_out"
if [[ "$hostname_out" != "$vm_name" ]]; then
  echo "unexpected guest hostname: $hostname_out" >&2
  exit 1
fi

step "exec stdin roundtrip"
stdin_out="$(printf 'hello-from-host\n' | kb exec "$vm_name" -- cat)"
printf 'state: stdinRoundtrip=%s\n' "$stdin_out"
if [[ "$stdin_out" != "hello-from-host" ]]; then
  echo "stdin roundtrip mismatch: $stdin_out" >&2
  exit 1
fi

step "exec env injection"
env_out="$(kb exec --env FOO=bar "$vm_name" -- sh -c 'printf %s "$FOO"')"
printf 'state: envFOO=%s\n' "$env_out"
if [[ "$env_out" != "bar" ]]; then
  echo "env injection mismatch: $env_out" >&2
  exit 1
fi

step "exec non-zero exit"
set +e
kb exec "$vm_name" -- sh -c 'echo expected-error >&2; exit 7' >/tmp/kumabox-exec-stdout.$$ 2>/tmp/kumabox-exec-stderr.$$
exit_status=$?
set -e
stderr_text="$(cat /tmp/kumabox-exec-stderr.$$)"
rm -f /tmp/kumabox-exec-stdout.$$ /tmp/kumabox-exec-stderr.$$
printf 'state: nonZeroExit=%s stderr=%s\n' "$exit_status" "$stderr_text"
if [[ "$exit_status" -ne 7 || "$stderr_text" != *"expected-error"* ]]; then
  echo "non-zero exit propagation failed" >&2
  exit 1
fi

run_elapsed_ms=$((run_done_ms - run_start_ms))
agent_wait_ms=$((agent_done_ms - agent_start_ms))
boot_exec_ms=$((agent_done_ms - run_start_ms))
printf 'metrics: runReturnMs=%d agentWaitMs=%d bootExecReadyMs=%d\n' \
  "$run_elapsed_ms" "$agent_wait_ms" "$boot_exec_ms"

step "delete VM and preserve managed image"
delete_json="$(kb delete "$vm_name" --force)"
printf '%s\n' "$delete_json"

script_status=0
echo "KumaBox OCI E2E verification passed"
