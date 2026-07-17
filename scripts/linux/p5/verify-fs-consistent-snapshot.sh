#!/usr/bin/env bash
set -euo pipefail

kumabox=./bin/kumabox
cloud_hypervisor=cloud-hypervisor
qemu_img=qemu-img
root_dir=/tmp/kumabox-p0/data
run_dir=/tmp/kumabox-p0/run
log_dir=/tmp/kumabox-p0/logs
image=p3-agent-image
source_name=p5-fs-source
clone_name=p5-fs-clone
snapshot_name=p5-fs-consistent
storage=64M
agent_timeout=180s
use_sudo=false
success=false

usage() {
  cat <<'EOF'
Usage: verify-fs-consistent-snapshot.sh [options]

  --kumabox PATH
  --cloud-hypervisor PATH
  --qemu-img PATH
  --root-dir PATH
  --run-dir PATH
  --log-dir PATH
  --image REF              managed OCI image rebuilt with the current agent
  --source-name NAME
  --clone-name NAME
  --snapshot NAME
  --storage SIZE
  --agent-timeout DURATION
  --sudo
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
    --clone-name) clone_name=$2; shift 2 ;;
    --snapshot) snapshot_name=$2; shift 2 ;;
    --storage) storage=$2; shift 2 ;;
    --agent-timeout) agent_timeout=$2; shift 2 ;;
    --sudo) use_sudo=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ $(uname -s) == Linux ]] || { echo "fs-consistent verification must run on Linux" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 1; }
if [[ $use_sudo == true ]]; then kb_prefix=(sudo); else kb_prefix=(); fi

step() { printf '\n==> %s\n' "$1"; }
kb() {
  "${kb_prefix[@]}" "$kumabox" \
    --root-dir "$root_dir" --run-dir "$run_dir" --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor" --qemu-img-bin "$qemu_img" "$@"
}
cleanup() {
  kb delete "$clone_name" --force >/dev/null 2>&1 || true
  kb delete "$source_name" --force >/dev/null 2>&1 || true
  kb snapshot rm "$snapshot_name" >/dev/null 2>&1 || true
}
on_exit() {
  [[ $success == true ]] && { cleanup; return; }
  step "preserving failed fs-consistent snapshot state"
  kb inspect "$source_name" --json 2>/dev/null || true
  kb inspect "$clone_name" --json 2>/dev/null || true
  kb snapshot inspect "$snapshot_name" --json 2>/dev/null || true
  if [[ -n ${source_id:-} ]]; then
    step "failure context: guest writable filesystem mounts"
    kb exec "$source_id" -- sh -c '
      printf "%s\n" "mountinfo (ext4, xfs, btrfs, and overlay):"
      grep -E " - (ext[234]|xfs|btrfs|overlay) " /proc/self/mountinfo || true
      printf "%s\n" "findmnt:"
      findmnt -rn -o TARGET,SOURCE,FSTYPE,OPTIONS || true
      printf "%s\n" "reserved COW mount:"
      findmnt -T /.kumabox/cow -o TARGET,SOURCE,FSTYPE,OPTIONS || true
    ' 2>&1 || true
  fi
  printf 'state: root_dir=%s run_dir=%s log_dir=%s\n' "$root_dir" "$run_dir" "$log_dir"
}
trap on_exit EXIT

step "clean previous verification state"
cleanup

step "environment checks"
scripts/linux/env-check.sh --kumabox "$kumabox" --cloud-hypervisor "$cloud_hypervisor" --qemu-img "$qemu_img" --strict

step "run source VM and verify current guest agent"
source_json=$(kb run "$image" --name "$source_name" --network none --storage "$storage")
source_id=$(jq -r '.id' <<<"$source_json")
printf '%s\n' "$source_json" | jq '{id,name,state,vsockSocket}'
kb agent ping "$source_id" --timeout "$agent_timeout" | jq .

step "write durable state while the guest is active"
kb exec "$source_id" -- sh -c 'printf "before-freeze\n" > /var/tmp/kumabox-fs-marker; sync'

step "capture with strict filesystem consistency"
snapshot_json=$(kb snapshot create "$source_id" --name "$snapshot_name" --type running --consistent fs)
snapshot_id=$(jq -r '.id' <<<"$snapshot_json")
printf '%s\n' "$snapshot_json" | jq .
manifest=$("${kb_prefix[@]}" jq . "$(jq -r '.dataDir' <<<"$snapshot_json")/snapshot.json")
jq -e '.type == "native" and .consistency == "fs"' <<<"$manifest" >/dev/null
printf '%s\n' "$manifest" | jq '{id,type,consistency,backend,machine,nativeFiles:(.native.files|length)}'

step "prove source resumed and filesystems were thawed"
kb exec "$source_id" -- sh -c 'printf "after-thaw\n" >> /var/tmp/kumabox-fs-marker; sync'
kb agent ping "$source_id" --timeout "$agent_timeout" | jq .

step "clone the snapshot and verify captured durable state"
clone_json=$(kb clone "$snapshot_id" --name "$clone_name" --network none --restore-mode copy)
clone_id=$(jq -r '.id' <<<"$clone_json")
printf '%s\n' "$clone_json" | jq '{id,name,state,lastRestore}'
kb agent ping "$clone_id" --timeout "$agent_timeout" | jq .
marker=$(kb exec "$clone_id" -- cat /var/tmp/kumabox-fs-marker)
printf 'state: cloned marker=%q\n' "$marker"
grep -q '^before-freeze$' <<<"$marker"
if grep -q '^after-thaw$' <<<"$marker"; then
  echo "post-snapshot write unexpectedly appeared in clone" >&2
  exit 1
fi

step "remove verification resources"
kb delete "$clone_id" --force | jq '{id,name,state}'
kb delete "$source_id" --force | jq '{id,name,state}'
kb snapshot rm "$snapshot_id" | jq .

success=true
echo "P5 fs-consistent snapshot verification passed"
