#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
cloud_hypervisor_path="cloud-hypervisor"
qemu_img_path="qemu-img"
root_dir="/tmp/kumabox-p0/data"
run_dir="/tmp/kumabox-p0/run"
log_dir="/tmp/kumabox-p0/logs"
image="ubuntu"
vm_name="p5-pause"
use_sudo=false
script_status=1

usage() {
  cat <<'USAGE'
Usage: scripts/linux/p5/verify-pause-resume.sh [options]

Options:
  --kumabox PATH             kumabox binary path, defaults to ./bin/kumabox
  --cloud-hypervisor PATH    cloud-hypervisor path, defaults to cloud-hypervisor
  --qemu-img PATH            qemu-img path, defaults to qemu-img
  --root-dir PATH            state root directory, defaults to /tmp/kumabox-p0/data
  --run-dir PATH             runtime directory, defaults to /tmp/kumabox-p0/run
  --log-dir PATH             log directory, defaults to /tmp/kumabox-p0/logs
  --image REF                existing managed image, defaults to ubuntu
  --name NAME                verification VM name, defaults to p5-pause
  --sudo                     run KumaBox and privileged inspection through sudo

The script preserves fixtures, images, and unrelated VMs. It removes only the
named verification VM before starting and after a successful verification.
USAGE
}

require_value() {
  if [[ -z "${2:-}" ]]; then
    echo "$1 requires a value" >&2
    exit 2
  fi
}

step() {
  printf '\n==> %s\n' "$1"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --kumabox) require_value "$1" "${2:-}"; kumabox_path="$2"; shift 2 ;;
    --cloud-hypervisor) require_value "$1" "${2:-}"; cloud_hypervisor_path="$2"; shift 2 ;;
    --qemu-img) require_value "$1" "${2:-}"; qemu_img_path="$2"; shift 2 ;;
    --root-dir) require_value "$1" "${2:-}"; root_dir="$2"; shift 2 ;;
    --run-dir) require_value "$1" "${2:-}"; run_dir="$2"; shift 2 ;;
    --log-dir) require_value "$1" "${2:-}"; log_dir="$2"; shift 2 ;;
    --image) require_value "$1" "${2:-}"; image="$2"; shift 2 ;;
    --name) require_value "$1" "${2:-}"; vm_name="$2"; shift 2 ;;
    --sudo) use_sudo=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "pause/resume verification must run on Linux" >&2
  exit 1
fi
for command in jq curl; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "$command is required for pause/resume verification" >&2
    exit 1
  fi
done
if [[ ! -x "$kumabox_path" ]]; then
  echo "kumabox is not executable: $kumabox_path" >&2
  exit 1
fi

if [[ "$use_sudo" == true ]]; then
  if ! command -v sudo >/dev/null 2>&1; then
    echo "--sudo requested but sudo is missing" >&2
    exit 1
  fi
  kumabox_cmd=(sudo "$kumabox_path")
  curl_cmd=(sudo curl)
  signal_cmd=(sudo kill)
  tail_cmd=(sudo tail)
else
  kumabox_cmd=("$kumabox_path")
  curl_cmd=(curl)
  signal_cmd=(kill)
  tail_cmd=(tail)
fi

kb() {
  "${kumabox_cmd[@]}" \
    --root-dir "$root_dir" \
    --run-dir "$run_dir" \
    --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor_path" \
    --qemu-img-bin "$qemu_img_path" \
    "$@"
}

cleanup() {
  kb delete "$vm_name" --force >/dev/null 2>&1 || true
}

on_exit() {
  if [[ "$script_status" -eq 0 ]]; then
    cleanup
    return
  fi
  printf '\n==> preserving failed P5-01 state\n' >&2
  kb inspect "$vm_name" --json 2>/dev/null || true
  if [[ -n "${events_log:-}" ]]; then
    "${tail_cmd[@]}" -n 40 "$events_log" 2>/dev/null || true
  fi
  printf 'state: root_dir=%s run_dir=%s log_dir=%s\n' "$root_dir" "$run_dir" "$log_dir" >&2
}
trap on_exit EXIT

step "clean previous verification VM"
cleanup

step "environment checks"
scripts/linux/env-check.sh \
  --kumabox "$kumabox_path" \
  --cloud-hypervisor "$cloud_hypervisor_path" \
  --qemu-img "$qemu_img_path" \
  --strict

step "inspect managed image"
kb image inspect "$image" --json | jq '{id, name, source, rootDisk, boot}'

step "run verification VM"
run_json="$(kb run "$image" --name "$vm_name" --network none)"
printf '%s\n' "$run_json"
vm_id="$(printf '%s' "$run_json" | jq -r '.id')"
pid="$(printf '%s' "$run_json" | jq -r '.pid')"
api_socket="$(printf '%s' "$run_json" | jq -r '.apiSocket')"
events_log="$(printf '%s' "$run_json" | jq -r '.logDir')/events.log"
"${signal_cmd[@]}" -0 "$pid"
printf 'state: vm=%s pid=%s api_socket=%s\n' "$vm_id" "$pid" "$api_socket"

backend_state() {
  "${curl_cmd[@]}" --silent --show-error --unix-socket "$api_socket" \
    http://localhost/api/v1/vm.info | jq -r '.state'
}

step "pause VM"
pause_json="$(kb pause "$vm_name")"
printf '%s\n' "$pause_json"
printf '%s' "$pause_json" | jq -e '.state == "paused" and .observedState == "PAUSED"' >/dev/null
"${signal_cmd[@]}" -0 "$pid"
state="$(backend_state)"
printf 'state: backend=%s pid=%s remains alive\n' "$state" "$pid"
if [[ "${state,,}" != "paused" ]]; then
  echo "Cloud Hypervisor did not report Paused" >&2
  exit 1
fi

step "repeat pause to verify idempotency"
kb pause "$vm_name" | jq '{id, state, observedState, pid, apiSocket}'

step "resume VM"
resume_json="$(kb resume "$vm_name")"
printf '%s\n' "$resume_json"
printf '%s' "$resume_json" | jq -e '.state == "running" and .observedState == "RUNNING"' >/dev/null
"${signal_cmd[@]}" -0 "$pid"
state="$(backend_state)"
printf 'state: backend=%s pid=%s remains alive\n' "$state" "$pid"
if [[ "${state,,}" != "running" ]]; then
  echo "Cloud Hypervisor did not report Running" >&2
  exit 1
fi

step "repeat resume to verify idempotency"
kb resume "$vm_name" | jq '{id, state, observedState, pid, apiSocket}'

step "inspect lifecycle events"
"${tail_cmd[@]}" -n 20 "$events_log"
if ! "${tail_cmd[@]}" -n 50 "$events_log" | jq -e 'select(.type == "backend.pause.completed")' >/dev/null; then
  echo "pause lifecycle event is missing" >&2
  exit 1
fi
if ! "${tail_cmd[@]}" -n 50 "$events_log" | jq -e 'select(.type == "backend.resume.completed")' >/dev/null; then
  echo "resume lifecycle event is missing" >&2
  exit 1
fi

step "stop and delete verification VM"
kb stop "$vm_name" | jq '{id, state, observedState}'
kb delete "$vm_name" | jq '{id, name, state}'

script_status=0
echo "P5-01 pause/resume verification passed"
