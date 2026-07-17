#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd -- "$script_dir/../../.." && pwd)

kumabox=./bin/kumabox
cloud_hypervisor=cloud-hypervisor
qemu_img=qemu-img
root_dir=/tmp/kumabox-p0/data
run_dir=/tmp/kumabox-p0/run
log_dir=/tmp/kumabox-p0/logs
image=p3-agent-image
memory=512M
storage=64M
agent_timeout=180s
use_sudo=false
suite_started=$SECONDS
completed=0

usage() {
  cat <<'EOF'
Usage: scripts/linux/p5/verify-e2e.sh [options]

  --kumabox PATH
  --cloud-hypervisor PATH
  --qemu-img PATH
  --root-dir PATH
  --run-dir PATH
  --log-dir PATH
  --image REF              managed OCI image containing the current guest agent
  --memory SIZE            compatibility-test guest memory, defaults to 512M
  --storage SIZE           per-VM writable storage, defaults to 64M
  --agent-timeout DURATION guest-agent timeout, defaults to 180s
  --sudo                   run KumaBox and privileged checks through sudo

Runs the complete P5 acceptance suite in dependency order. Each scenario
removes only its named resources after success. A failed scenario preserves its
VM, snapshot, runtime files, and logs, then stops the suite immediately.
EOF
}

require_value() {
  [[ -n ${2:-} ]] || {
    echo "$1 requires a value" >&2
    exit 2
  }
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
    --memory) require_value "$1" "${2:-}"; memory=$2; shift 2 ;;
    --storage) require_value "$1" "${2:-}"; storage=$2; shift 2 ;;
    --agent-timeout) require_value "$1" "${2:-}"; agent_timeout=$2; shift 2 ;;
    --sudo) use_sudo=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ $(uname -s) == Linux ]] || {
  echo "P5 E2E verification must run on Linux" >&2
  exit 1
}
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 1; }
if [[ $use_sudo == true ]]; then
  command -v sudo >/dev/null 2>&1 || { echo "--sudo requested but sudo is missing" >&2; exit 1; }
  kb_prefix=(sudo)
  sudo_args=(--sudo)
else
  kb_prefix=()
  sudo_args=()
fi

cd -- "$repo_root"
[[ -x $kumabox ]] || { echo "kumabox is not executable: $kumabox" >&2; exit 1; }

common_args=(
  --kumabox "$kumabox"
  --cloud-hypervisor "$cloud_hypervisor"
  --qemu-img "$qemu_img"
  --root-dir "$root_dir"
  --run-dir "$run_dir"
  --log-dir "$log_dir"
  --image "$image"
)

run_case() {
  local label=$1 script=$2
  shift 2
  local started=$SECONDS status
  printf '\n================================================================\n'
  printf '==> %s\n' "$label"
  printf '================================================================\n'
  if "$script_dir/$script" "$@"; then
    completed=$((completed + 1))
    printf '\npass: %s (%ss)\n' "$label" "$((SECONDS - started))"
    return 0
  else
    status=$?
    printf '\nFAIL: %s exited with status %s after %ss\n' "$label" "$status" "$((SECONDS - started))" >&2
    printf 'state: completed=%s/9 root_dir=%s run_dir=%s log_dir=%s\n' \
      "$completed" "$root_dir" "$run_dir" "$log_dir" >&2
    return "$status"
  fi
}

printf 'P5 E2E configuration:\n'
printf '  image:    %s\n' "$image"
printf '  rootDir:  %s\n' "$root_dir"
printf '  runDir:   %s\n' "$run_dir"
printf '  logDir:   %s\n' "$log_dir"
printf '  memory:   %s\n' "$memory"
printf '  storage:  %s\n' "$storage"
printf '  sudo:     %s\n' "$use_sudo"

printf '\n==> preflight managed OCI agent image\n'
image_json=$("${kb_prefix[@]}" "$kumabox" \
  --root-dir "$root_dir" --run-dir "$run_dir" --log-dir "$log_dir" \
  --cloud-hypervisor-bin "$cloud_hypervisor" --qemu-img-bin "$qemu_img" \
  image inspect "$image" --json)
printf '%s\n' "$image_json" | jq '{id,name,source,boot,os,agentInjection:.oci.agentInjection}'
jq -e '.boot.mode == "direct" and .oci != null' <<<"$image_json" >/dev/null || {
  echo "P5 E2E requires a managed direct-boot OCI image with the current guest agent" >&2
  exit 1
}

run_case "P5-01 Pause/Resume" verify-pause-resume.sh \
  "${common_args[@]}" --name p5-pause "${sudo_args[@]}"

run_case "P5-02 Native Capture Transaction" verify-native-capture.sh \
  "${common_args[@]}" --name p5-native --snapshot p5-native-running \
  --storage "$storage" "${sudo_args[@]}"

run_case "P5-03 Native Manifest and Compatibility" verify-native-compatibility.sh \
  "${common_args[@]}" --name p5-compat --snapshot p5-compat-native \
  --memory "$memory" --storage "$storage" "${sudo_args[@]}"

run_case "P5-04 In-place Restore" verify-native-restore.sh \
  "${common_args[@]}" --name p5-restore --snapshot p5-restore-native \
  --storage "$storage" --agent-timeout "$agent_timeout" "${sudo_args[@]}"

run_case "P5-05 Clone Identity and Datapath" verify-native-clone.sh \
  "${common_args[@]}" --source-name p5-clone-source --clone-name p5-clone-target \
  --snapshot p5-clone-native --storage "$storage" --agent-timeout "$agent_timeout" \
  "${sudo_args[@]}"

run_case "P5-06 Restore Memory Modes" verify-memory-restore-modes.sh \
  "${common_args[@]}" --source-name p5-memory-source --storage "$storage" \
  --agent-timeout "$agent_timeout" "${sudo_args[@]}"

run_case "P5-07 Filesystem-consistent Snapshot" verify-fs-consistent-snapshot.sh \
  "${common_args[@]}" --source-name p5-fs-source --clone-name p5-fs-clone \
  --snapshot p5-fs-consistent --storage "$storage" --agent-timeout "$agent_timeout" \
  "${sudo_args[@]}"

run_case "P5-08 Hibernate and Restore" verify-hibernate-restore.sh \
  "${common_args[@]}" --name p5-hibernate --snapshot p5-hibernate-nap \
  --storage "$storage" --agent-timeout "$agent_timeout" "${sudo_args[@]}"

run_case "P5-09 Native Snapshot GC" verify-native-snapshot-gc.sh \
  "${common_args[@]}" --name p5-gc-source --snapshot p5-gc-native \
  --storage "$storage" "${sudo_args[@]}"

printf '\n================================================================\n'
printf 'P5 E2E verification passed: %s/9 scenarios in %ss\n' "$completed" "$((SECONDS - suite_started))"
printf '================================================================\n'
