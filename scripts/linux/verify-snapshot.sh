#!/usr/bin/env bash
set -euo pipefail

kumabox=./bin/kumabox
cloud_hypervisor=cloud-hypervisor
qemu_img=qemu-img
root_dir=/tmp/kumabox-p0/data
run_dir=/tmp/kumabox-p0/run
log_dir=/tmp/kumabox-p0/logs
image=p3-agent-image-v3
scenario=all
storage=64M
agent_timeout=180s
package=/tmp/kumabox-p0/snapshot-e2e.kbsnap
use_sudo=false
success=false
active_case=preflight

usage() {
  cat <<'EOF'
Usage: scripts/linux/verify-snapshot.sh [options]

  --kumabox PATH
  --cloud-hypervisor PATH
  --qemu-img PATH
  --root-dir PATH
  --run-dir PATH
  --log-dir PATH
  --image NAME
  --scenario all|stopped|native|native-lifetime|hibernate
  --storage SIZE
  --agent-timeout DURATION
  --package PATH
  --sudo

Runs the high-value snapshot E2E scenarios against one managed direct-boot OCI
image. Failed state is preserved. Successful scenarios remove their own VMs,
snapshots, and portable package.
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
    --scenario) require_value "$1" "${2:-}"; scenario=$2; shift 2 ;;
    --storage) require_value "$1" "${2:-}"; storage=$2; shift 2 ;;
    --agent-timeout) require_value "$1" "${2:-}"; agent_timeout=$2; shift 2 ;;
    --package) require_value "$1" "${2:-}"; package=$2; shift 2 ;;
    --sudo) use_sudo=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

case $scenario in all|stopped|native|native-lifetime|hibernate) ;; *) echo "invalid --scenario: $scenario" >&2; exit 2 ;; esac
[[ $(uname -s) == Linux ]] || { echo "snapshot E2E requires Linux" >&2; exit 1; }
for binary in jq "$cloud_hypervisor" "$qemu_img"; do
  command -v "$binary" >/dev/null 2>&1 || { echo "required command not found: $binary" >&2; exit 1; }
done
[[ -x $kumabox ]] || { echo "kumabox is not executable: $kumabox" >&2; exit 1; }

if [[ $use_sudo == true ]]; then
  command -v sudo >/dev/null 2>&1 || { echo "sudo is required by --sudo" >&2; exit 1; }
  sudo -v
  kb_prefix=(sudo)
  file_prefix=(sudo)
else
  kb_prefix=()
  file_prefix=()
fi

kb() {
  "${kb_prefix[@]}" "$kumabox" \
    --root-dir "$root_dir" --run-dir "$run_dir" --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor" --qemu-img-bin "$qemu_img" "$@"
}
step() { printf '\n==> %s\n' "$1"; }
remove_file() { "${file_prefix[@]}" rm -f "$1"; }

vm_names=(snapshot-stopped-source snapshot-stopped-restored snapshot-native-source snapshot-native-clone snapshot-native-lifetime-source snapshot-native-lifetime-clone snapshot-hibernate)
snapshot_names=(snapshot-stopped-disk snapshot-stopped-imported snapshot-native-running snapshot-native-lifetime snapshot-hibernate-running)

cleanup_all() {
  local ref
  for ref in "${vm_names[@]}"; do kb delete "$ref" --force >/dev/null 2>&1 || true; done
  for ref in "${snapshot_names[@]}"; do kb snapshot rm "$ref" >/dev/null 2>&1 || true; done
  remove_file "$package" >/dev/null 2>&1 || true
}

failure_context() {
  local ref
  step "failure context: $active_case"
  for ref in "${vm_names[@]}"; do
    if kb inspect "$ref" --json >/tmp/kumabox-snapshot-inspect.$$ 2>/dev/null; then
      jq '{id,name,state,observedState,pid,lastRestore,hibernate,networkConfigs,storageConfigs}' /tmp/kumabox-snapshot-inspect.$$ || true
      kb logs "$ref" --source stderr --tail 80 2>/dev/null || true
      kb logs "$ref" --source console --tail 80 2>/dev/null || true
    fi
  done
  rm -f /tmp/kumabox-snapshot-inspect.$$
  for ref in "${snapshot_names[@]}"; do kb snapshot inspect "$ref" --json 2>/dev/null || true; done
  printf 'state: root_dir=%s run_dir=%s log_dir=%s package=%s\n' "$root_dir" "$run_dir" "$log_dir" "$package"
}

on_exit() {
  local status=$?
  if [[ $success == true ]]; then
    cleanup_all
    return
  fi
  failure_context >&2
  printf 'snapshot E2E failed in %s with status %s; state preserved\n' "$active_case" "$status" >&2
}
trap on_exit EXIT

verify_stopped() {
  local source=snapshot-stopped-source restored=snapshot-stopped-restored
  local disk_snapshot=snapshot-stopped-disk imported=snapshot-stopped-imported
  active_case=stopped
  step "stopped: run source and write durable state"
  source_json=$(kb run "$image" --name "$source" --network none --storage "$storage")
  source_id=$(jq -r '.id' <<<"$source_json")
  kb agent ping "$source_id" --timeout "$agent_timeout" | jq .
  kb exec "$source_id" -- sh -c 'printf stopped-state > /var/tmp/kumabox-stopped-marker; sync'

  step "stopped: capture, export, and import writable state"
  kb stop "$source_id" | jq '{id,name,state}'
  snapshot_json=$(kb snapshot create "$source_id" --name "$disk_snapshot")
  snapshot_id=$(jq -r '.id' <<<"$snapshot_json")
  kb snapshot export "$snapshot_id" --output "$package" --compression none | jq .
  import_json=$(kb snapshot import "$package" --name "$imported")
  imported_id=$(jq -r '.id' <<<"$import_json")

  step "stopped: restore through normal lifecycle"
  restore_json=$(kb snapshot restore "$imported_id" --name "$restored" --network none)
  restored_id=$(jq -r '.id' <<<"$restore_json")
  [[ $restored_id != "$source_id" ]] || { echo "stopped restore reused source VM identity" >&2; return 1; }
  kb start "$restored_id" | jq '{id,name,state}'
  kb agent ping "$restored_id" --timeout "$agent_timeout" | jq .
  [[ $(kb exec "$restored_id" -- cat /var/tmp/kumabox-stopped-marker) == stopped-state ]]

  kb delete "$restored_id" --force >/dev/null
  kb delete "$source_id" --force >/dev/null
  kb snapshot rm "$imported_id" >/dev/null
  kb snapshot rm "$snapshot_id" >/dev/null
  remove_file "$package"
  echo "pass: stopped snapshot export/import/restore"
}

verify_native() {
  local source=snapshot-native-source clone=snapshot-native-clone snapshot=snapshot-native-running
  active_case=native
  step "native: run source and create memory plus disk state"
  source_json=$(kb run "$image" --name "$source" --network none --storage "$storage")
  source_id=$(jq -r '.id' <<<"$source_json")
  source_vsock=$(jq -r '.vsockSocket' <<<"$source_json")
  source_disk=$(jq -r '[.storageConfigs[] | select(.role == "cow")][0].path' <<<"$source_json")
  kb agent ping "$source_id" --timeout "$agent_timeout" >/dev/null
  guest_pid=$(kb exec "$source_id" -- sh -c 'nohup sh -c '\''while :; do sleep 1; done'\'' >/dev/null 2>&1 & echo $!')
  kb exec "$source_id" -- sh -c 'printf native-memory > /run/kumabox-native-marker; printf native-disk > /var/tmp/kumabox-native-marker; sync'

  step "native: capture and clone running state"
  snapshot_json=$(kb snapshot create "$source_id" --name "$snapshot" --type running)
  snapshot_id=$(jq -r '.id' <<<"$snapshot_json")
  snapshot_inspect=$(kb snapshot inspect "$snapshot_id" --json)
  jq -e '
    .state == "ready" and
    (.performance != null) and
    (.performance.pauseDurationMs >= 0) and
    (.performance.nativeCaptureMs >= 0) and
    (.performance.writableDiskStageMs >= 0) and
    (.performance.publicationDurationMs >= 0) and
    (.performance.totalDurationMs >= .performance.pauseDurationMs)
  ' <<<"$snapshot_inspect" >/dev/null || {
    echo "native snapshot performance metrics are incomplete" >&2
    jq '{id,name,state,performance}' <<<"$snapshot_inspect" >&2
    return 1
  }
  jq '{id,name,state,performance}' <<<"$snapshot_inspect"
  clone_json=$(kb clone "$snapshot_id" --name "$clone" --network none --restore-mode copy)
  clone_id=$(jq -r '.id' <<<"$clone_json")
  clone_vsock=$(jq -r '.vsockSocket' <<<"$clone_json")
  clone_disk=$(jq -r '[.storageConfigs[] | select(.role == "cow")][0].path' <<<"$clone_json")
  [[ $clone_id != "$source_id" && $clone_vsock != "$source_vsock" && $clone_disk != "$source_disk" ]]
  clone_inspect=$(kb inspect "$clone_id" --json)
  jq -e '
    .state == "running" and
    (.lastRestore != null) and
    (.lastRestore.mode == "copy") and
    (.lastRestore.nativeStageDurationMs >= 0) and
    (.lastRestore.diskStageDurationMs >= 0) and
    (.lastRestore.diskCommitDurationMs >= 0) and
    (.lastRestore.backendRestoreDurationMs >= 0) and
    (.lastRestore.identityDurationMs >= 0) and
    (.lastRestore.readinessDurationMs >= 0) and
    (.lastRestore.durationMs >= .lastRestore.readinessDurationMs)
  ' <<<"$clone_inspect" >/dev/null || {
    echo "native clone restore metrics are incomplete" >&2
    jq '{id,name,state,lastRestore}' <<<"$clone_inspect" >&2
    return 1
  }
  jq '{id,name,state,lastRestore}' <<<"$clone_inspect"
  kb agent ping "$clone_id" --timeout "$agent_timeout" >/dev/null
  kb exec "$clone_id" -- sh -c "kill -0 $guest_pid; test \"\$(cat /run/kumabox-native-marker)\" = native-memory; test \"\$(cat /var/tmp/kumabox-native-marker)\" = native-disk"
  [[ $(kb exec "$clone_id" -- uname -n) == "$clone" ]]
  kb agent ping "$source_id" --timeout "$agent_timeout" >/dev/null
  [[ $(kb exec "$source_id" -- cat /var/tmp/kumabox-native-marker) == native-disk ]]

  kb delete "$clone_id" --force >/dev/null
  kb delete "$source_id" --force >/dev/null
  kb snapshot rm "$snapshot_id" >/dev/null
  echo "pass: native snapshot clone continuity and identity"
}

verify_native_lifetime() {
  local source=snapshot-native-lifetime-source clone=snapshot-native-lifetime-clone snapshot=snapshot-native-lifetime
  local source_json snapshot_json snapshot_id clone_json clone_id remove_output
  active_case=native-lifetime

  step "native-lifetime: run source and capture running snapshot"
  source_json=$(kb run "$image" --name "$source" --network none --storage "$storage")
  source_id=$(jq -r '.id' <<<"$source_json")
  kb agent ping "$source_id" --timeout "$agent_timeout" >/dev/null
  kb exec "$source_id" -- sh -c 'printf native-lifetime > /var/tmp/kumabox-native-lifetime; sync'
  snapshot_json=$(kb snapshot create "$source_id" --name "$snapshot" --type running)
  snapshot_id=$(jq -r '.id' <<<"$snapshot_json")
  [[ -n $snapshot_id && $snapshot_id != null ]] || { echo "native snapshot id is missing" >&2; return 1; }

  step "native-lifetime: restore clone with OnDemand memory"
  clone_json=$(kb clone "$snapshot_id" --name "$clone" --network none --restore-mode ondemand)
  clone_id=$(jq -r '.id' <<<"$clone_json")
  kb agent ping "$clone_id" --timeout "$agent_timeout" >/dev/null
  jq -e '.lastRestore.mode == "ondemand" and .lastRestore.durationMs >= 0' \
    < <(kb inspect "$clone_id" --json) >/dev/null || {
      echo "OnDemand clone restore metadata is incomplete" >&2
      kb inspect "$clone_id" --json >&2 || true
      return 1
    }

  step "native-lifetime: source snapshot must stay pinned while clone runs"
  if remove_output=$(kb snapshot rm "$snapshot_id" 2>&1); then
    echo "snapshot deletion unexpectedly succeeded while OnDemand clone was running" >&2
    printf '%s\n' "$remove_output" >&2
    return 1
  fi
  printf 'pass: source snapshot deletion rejected while clone is alive\n'
  printf '%s\n' "$remove_output" | sed -n '1,3p'

  step "native-lifetime: release clone and delete source snapshot"
  kb delete "$clone_id" --force >/dev/null
  kb snapshot rm "$snapshot_id" >/dev/null
  kb delete "$source_id" --force >/dev/null
  echo "pass: OnDemand snapshot lifetime pin released after clone deletion"
}

verify_hibernate() {
  local name=snapshot-hibernate snapshot=snapshot-hibernate-running
  active_case=hibernate
  step "hibernate: create memory and disk state"
  run_json=$(kb run "$image" --name "$name" --network none --storage "$storage")
  vm_id=$(jq -r '.id' <<<"$run_json")
  kb agent ping "$vm_id" --timeout "$agent_timeout" >/dev/null
  guest_pid=$(kb exec "$vm_id" -- sh -c 'nohup sh -c '\''while :; do sleep 1; done'\'' >/dev/null 2>&1 & echo $!')
  kb exec "$vm_id" -- sh -c 'printf hibernate-memory > /run/kumabox-hibernate-marker; printf hibernate-disk > /var/tmp/kumabox-hibernate-marker; sync'

  step "hibernate: stop without resume gap and wake original identity"
  hibernate_json=$(kb hibernate "$vm_id" --name "$snapshot")
  snapshot_id=$(jq -r '.snapshot.id' <<<"$hibernate_json")
  jq -e '.vm.state == "stopped" and .vm.hibernate != null' <<<"$hibernate_json" >/dev/null
  restore_json=$(kb restore "$vm_id" "$snapshot_id" --restore-mode copy)
  jq -e '.state == "running" and .hibernate == null' <<<"$restore_json" >/dev/null
  kb agent ping "$vm_id" --timeout "$agent_timeout" >/dev/null
  kb exec "$vm_id" -- sh -c "kill -0 $guest_pid; test \"\$(cat /run/kumabox-hibernate-marker)\" = hibernate-memory; test \"\$(cat /var/tmp/kumabox-hibernate-marker)\" = hibernate-disk"

  kb delete "$vm_id" --force >/dev/null
  kb snapshot rm "$snapshot_id" >/dev/null
  echo "pass: hibernate and restore"
}

step "clean named snapshot verification resources"
cleanup_all
step "verify managed direct-boot OCI image"
kb image inspect "$image" --json | jq -e '.boot.mode == "direct" and .oci != null' >/dev/null
scripts/linux/env-check.sh --kumabox "$kumabox" --cloud-hypervisor "$cloud_hypervisor" --qemu-img "$qemu_img" --strict

case $scenario in
  stopped) verify_stopped ;;
  native) verify_native ;;
  native-lifetime) verify_native_lifetime ;;
  fs) verify_fs ;;
  hibernate) verify_hibernate ;;
  all) verify_stopped; verify_native; verify_hibernate ;;
esac

success=true
echo "KumaBox snapshot E2E verification passed (scenario=$scenario)"
