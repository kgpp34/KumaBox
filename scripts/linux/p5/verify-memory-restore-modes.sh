#!/usr/bin/env bash
set -euo pipefail

kumabox=./bin/kumabox
cloud_hypervisor=cloud-hypervisor
qemu_img=qemu-img
root_dir=/tmp/kumabox-p0/data
run_dir=/tmp/kumabox-p0/run
log_dir=/tmp/kumabox-p0/logs
image=p3-agent-image
source_name=p5-memory-source
storage=64M
agent_timeout=180s
use_sudo=false
success=false
source_id=
active_clones=()
active_snapshots=()

usage() {
  cat <<'EOF'
Usage: verify-memory-restore-modes.sh [options]

  --kumabox PATH
  --cloud-hypervisor PATH
  --qemu-img PATH
  --root-dir PATH
  --run-dir PATH
  --log-dir PATH
  --image REF
  --source-name NAME
  --storage SIZE
  --agent-timeout DURATION
  --sudo

Verifies copy independence, delayed-mode capability gating, durable snapshot
pinning, restore latency records, and pin release after VM stop.
EOF
}

while (($#)); do
  case "$1" in
    --kumabox) kumabox=$2; shift 2 ;;
    --cloud-hypervisor) cloud_hypervisor=$2; shift 2 ;;
    --qemu-img) qemu_img=$2; shift 2 ;;
    --root-dir) root_dir=$2; shift 2 ;;
    --run-dir) run_dir=$2; shift 2 ;;
    --log-dir) log_dir=$2; shift 2 ;;
    --image) image=$2; shift 2 ;;
    --source-name) source_name=$2; shift 2 ;;
    --storage) storage=$2; shift 2 ;;
    --agent-timeout) agent_timeout=$2; shift 2 ;;
    --sudo) use_sudo=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ $(uname -s) == Linux ]] || { echo "memory restore verification must run on Linux" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 1; }
if [[ $use_sudo == true ]]; then
  command -v sudo >/dev/null 2>&1 || { echo "--sudo requested but sudo is missing" >&2; exit 1; }
  kb_prefix=(sudo)
else
  kb_prefix=()
fi

step() { printf '\n==> %s\n' "$1"; }
kb() {
  "${kb_prefix[@]}" "$kumabox" \
    --root-dir "$root_dir" --run-dir "$run_dir" --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor" --qemu-img-bin "$qemu_img" "$@"
}
remove_value() {
  local value=$1
  shift
  local item
  for item in "$@"; do
    [[ $item == "$value" ]] || printf '%s\n' "$item"
  done
}
clean_named_state() {
  local mode
  for mode in copy ondemand mmap; do
    kb delete "p5-memory-$mode" --force >/dev/null 2>&1 || true
    kb snapshot rm "p5-memory-$mode" >/dev/null 2>&1 || true
  done
  kb delete "$source_name" --force >/dev/null 2>&1 || true
}
on_exit() {
  if [[ $success == true ]]; then
    clean_named_state
    return
  fi
  step "preserving failed memory restore state"
  [[ -z $source_id ]] || kb inspect "$source_id" --json 2>/dev/null || true
  local id
  for id in "${active_clones[@]}"; do
    kb inspect "$id" --json 2>/dev/null || true
  done
  for id in "${active_snapshots[@]}"; do
    kb snapshot inspect "$id" --json 2>/dev/null || true
  done
  printf 'state: root_dir=%s run_dir=%s log_dir=%s\n' "$root_dir" "$run_dir" "$log_dir"
}
trap on_exit EXIT

step "clean previous named verification resources"
clean_named_state

step "environment checks"
scripts/linux/env-check.sh \
  --kumabox "$kumabox" --cloud-hypervisor "$cloud_hypervisor" --qemu-img "$qemu_img" --strict

step "run source VM"
source_json=$(kb run "$image" --name "$source_name" --network none --storage "$storage")
source_id=$(jq -r '.id' <<<"$source_json")
printf '%s\n' "$source_json" | jq '{id,name,state,vsockSocket,memoryBytes}'
kb agent ping "$source_id" --timeout "$agent_timeout" | jq .

step "capture independent snapshots for all restore modes"
for mode in copy ondemand mmap; do
  snapshot_json=$(kb snapshot create "$source_id" --name "p5-memory-$mode" --type running --consistent crash)
  snapshot_id=$(jq -r '.id' <<<"$snapshot_json")
  active_snapshots+=("$snapshot_id")
  printf 'state: mode=%s snapshot=%s\n' "$mode" "$snapshot_id"
done

step "copy mode becomes independent from its snapshot"
copy_json=$(kb clone p5-memory-copy --name p5-memory-copy --network none --restore-mode copy)
copy_id=$(jq -r '.id' <<<"$copy_json")
active_clones+=("$copy_id")
printf '%s\n' "$copy_json" | jq '{id,name,state,lastRestore,snapshotDependency}'
jq -e '.lastRestore.mode == "copy" and (.lastRestore.durationMs >= 0) and (.snapshotDependency == null)' <<<"$copy_json" >/dev/null
kb snapshot rm p5-memory-copy | jq .
mapfile -t active_snapshots < <(remove_value "$(jq -r '.lastRestore.snapshotId' <<<"$copy_json")" "${active_snapshots[@]}")
kb agent ping "$copy_id" --timeout "$agent_timeout" | jq .
kb delete "$copy_id" --force | jq '{id,name,state}'
active_clones=()

verify_delayed_mode() {
  local mode=$1
  local clone_name="p5-memory-$mode"
  local stdout_file stderr_file remove_stdout remove_stderr clone_json clone_id snapshot_id stopped_json
  stdout_file=$(mktemp)
  stderr_file=$(mktemp)
  if ! kb clone "$clone_name" --name "$clone_name" --network none --restore-mode "$mode" >"$stdout_file" 2>"$stderr_file"; then
    if grep -q 'RESTORE_MODE_UNSUPPORTED' "$stderr_file"; then
      printf 'pass: %s is explicitly rejected by this Cloud Hypervisor build\n' "$mode"
      cat "$stderr_file"
      kb snapshot rm "$clone_name" | jq .
      rm -f "$stdout_file" "$stderr_file"
      return
    fi
    cat "$stderr_file" >&2
    rm -f "$stdout_file" "$stderr_file"
    return 1
  fi
  clone_json=$(<"$stdout_file")
  rm -f "$stdout_file" "$stderr_file"
  clone_id=$(jq -r '.id' <<<"$clone_json")
  snapshot_id=$(jq -r '.lastRestore.snapshotId' <<<"$clone_json")
  active_clones+=("$clone_id")
  printf '%s\n' "$clone_json" | jq '{id,name,state,lastRestore,snapshotDependency}'
  jq -e --arg mode "$mode" --arg snapshot "$snapshot_id" '
    .lastRestore.mode == $mode and
    (.lastRestore.durationMs >= 0) and
    .snapshotDependency.mode == $mode and
    .snapshotDependency.snapshotId == $snapshot
  ' <<<"$clone_json" >/dev/null
  kb agent ping "$clone_id" --timeout "$agent_timeout" | jq .

  remove_stdout=$(mktemp)
  remove_stderr=$(mktemp)
  if kb snapshot rm "$snapshot_id" >"$remove_stdout" 2>"$remove_stderr"; then
    echo "$mode snapshot was deleted while the clone depended on it" >&2
    rm -f "$remove_stdout" "$remove_stderr"
    return 1
  fi
  if ! grep -q 'SNAPSHOT_IN_USE' "$remove_stderr"; then
    cat "$remove_stderr" >&2
    rm -f "$remove_stdout" "$remove_stderr"
    return 1
  fi
  rm -f "$remove_stdout" "$remove_stderr"
  printf 'pass: %s snapshot deletion blocked with SNAPSHOT_IN_USE\n' "$mode"

  kb stop "$clone_id" | jq '{id,name,state,snapshotDependency}'
  stopped_json=$(kb inspect "$clone_id" --json)
  jq -e '.state == "stopped" and (.snapshotDependency == null)' <<<"$stopped_json" >/dev/null
  kb snapshot rm "$snapshot_id" | jq .
  kb delete "$clone_id" --force | jq '{id,name,state}'
  active_clones=()
}

step "verify ondemand capability and lifetime pin"
verify_delayed_mode ondemand

step "verify mmap capability and lifetime pin"
verify_delayed_mode mmap

step "remove source VM"
kb delete "$source_id" --force | jq '{id,name,state}'
source_id=

success=true
echo "P5 memory restore modes verification passed"
