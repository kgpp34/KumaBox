#!/usr/bin/env bash
set -euo pipefail

kumabox=./bin/kumabox
cloud_hypervisor=cloud-hypervisor
qemu_img=qemu-img
root_dir=/tmp/kumabox-p0/data
run_dir=/tmp/kumabox-p0/run
log_dir=/tmp/kumabox-p0/logs
image=p3-agent-image-v3
image_ref=kumabox/ubuntu:24.04-p6
network=cni:default
storage=64M
iterations=3
concurrency=1
output=/tmp/kumabox-p0/p6-baseline.json
artifacts_dir=
baseline=
max_p50_regression=10
max_p95_regression=15
use_sudo=false
run_timeout=20s
prepare_image=true

usage() {
  cat <<'EOF'
Usage: scripts/linux/benchmark-p6.sh [options]

Measures the default OCI+CNI lifecycle and writes one JSON benchmark record.
Each iteration covers cold run, first exec, stop, stopped snapshot/restore/start,
native running snapshot, and native clone.

  --kumabox PATH
  --cloud-hypervisor PATH
  --qemu-img PATH
  --root-dir PATH
  --run-dir PATH
  --log-dir PATH
  --image NAME              managed OCI image used by the benchmark
  --image-ref REF           local daemon image used to prepare NAME
  --network NAME            defaults to cni:default
  --storage SIZE            defaults to 64M
  --iterations N            defaults to 3
  --concurrency N           additional parallel OCI+CNI run batch, defaults to 1
  --output PATH             defaults to /tmp/kumabox-p0/p6-baseline.json
  --baseline PATH           compare against a previous JSON baseline
  --max-p50-regression N    defaults to 10 percent
  --max-p95-regression N    defaults to 15 percent
  --run-timeout DURATION    timeout for each VM lifecycle operation, defaults to 20s
  --skip-image-prepare     use NAME as-is without rebuilding the OCI image
  --sudo
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
    --iterations) require_value "$1" "${2:-}"; iterations=$2; shift 2 ;;
    --concurrency) require_value "$1" "${2:-}"; concurrency=$2; shift 2 ;;
    --output) require_value "$1" "${2:-}"; output=$2; shift 2 ;;
    --baseline) require_value "$1" "${2:-}"; baseline=$2; shift 2 ;;
    --max-p50-regression) require_value "$1" "${2:-}"; max_p50_regression=$2; shift 2 ;;
    --max-p95-regression) require_value "$1" "${2:-}"; max_p95_regression=$2; shift 2 ;;
    --run-timeout) require_value "$1" "${2:-}"; run_timeout=$2; shift 2 ;;
    --skip-image-prepare) prepare_image=false; shift ;;
    --sudo) use_sudo=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

artifacts_dir="${output}.logs"
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

[[ $(uname -s) == Linux ]] || { echo "P6 benchmark requires Linux" >&2; exit 1; }
[[ $iterations =~ ^[1-9][0-9]*$ ]] || { echo "--iterations must be positive" >&2; exit 2; }
[[ $concurrency =~ ^[1-9][0-9]*$ ]] || { echo "--concurrency must be positive" >&2; exit 2; }
if [[ $network == default ]]; then
  echo "benchmark requires CNI networking; use --network cni:<config-name> instead of --network default" >&2
  exit 2
fi
[[ -x $kumabox ]] || { echo "kumabox is not executable: $kumabox" >&2; exit 1; }
for command in jq make "$cloud_hypervisor" "$qemu_img"; do
  command -v "$command" >/dev/null 2>&1 || { echo "required command not found: $command" >&2; exit 1; }
done

if [[ $use_sudo == true ]]; then
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

build_project() {
  local build_user
  if [[ $(id -u) -eq 0 ]]; then
    build_user=${SUDO_USER:-}
    [[ -n "$build_user" ]] || { echo 'run as a normal user or use sudo from a normal user' >&2; exit 1; }
    sudo -iu "$build_user" bash -lic "cd '$repo_dir' && make build"
  else
    make -C "$repo_dir" build
  fi
}

now_ms() { date +%s%3N; }
step() { printf '==> %s\n' "$1" >&2; }

phase_file_marker_ms() {
  local phase_log=$1 pattern=$2 line timestamp
  line=$(grep -m1 -E "$pattern" <<<"$phase_log" || true)
  [[ -n $line ]] || { printf 'null'; return; }
  timestamp=$(sed -nE 's/.*monotonic-ms=([0-9]+).*/\1/p' <<<"$line")
  [[ -n $timestamp ]] || { printf 'null'; return; }
  printf '%s' "$timestamp"
}

journal_marker_ms() {
  local journal=$1 pattern=$2 line timestamp
  line=$(grep -m1 -E "$pattern" <<<"$journal" || true)
  [[ -n $line ]] || { printf 'null'; return; }
  timestamp=$(sed -nE 's/^\[[[:space:]]*([0-9]+\.[0-9]+)\].*/\1/p' <<<"$line")
  [[ -n $timestamp ]] || { printf 'null'; return; }
  awk -v seconds="$timestamp" 'BEGIN { printf "%.0f", seconds * 1000 }'
}

preserve_text() {
  local content=$1 destination=$2
  if ((${#file_prefix[@]})); then
    printf '%s\n' "$content" | "${file_prefix[@]}" tee "$destination" >/dev/null
    "${file_prefix[@]}" chmod a+r "$destination"
  else
    printf '%s\n' "$content" >"$destination"
  fi
}

phase_duration_ms() {
  local start=$1 end=$2
  if [[ $start =~ ^[0-9]+$ && $end =~ ^[0-9]+$ && $end -ge $start ]]; then
    printf '%s' "$((end - start))"
  else
    printf 'null'
  fi
}

read_guest_startup_journal() {
  local vm_ref=$1 attempt journal
  for ((attempt = 1; attempt <= 20; attempt++)); do
    journal=$(kb exec "$vm_ref" -- sh -c 'journalctl -b -o short-monotonic --no-pager | grep -E "systemd 255.*running in system mode|Started kumabox-agent.service|Reached target multi-user.target"' 2>/dev/null || true)
    if grep -q 'Reached target multi-user.target' <<<"$journal"; then
      printf '%s\n' "$journal"
      return 0
    fi
    sleep 0.2
  done
  printf '%s\n' "$journal"
}

preserve_console_log() {
  local source_log=$1 destination=$2
  [[ -f $source_log ]] || return 0
  if ((${#file_prefix[@]})); then
    "${file_prefix[@]}" cp "$source_log" "$destination"
    "${file_prefix[@]}" chmod a+r "$destination"
  else
    cp "$source_log" "$destination"
  fi
}

print_cold_run_context() {
  local vm_log_dir
  printf '\n==> cold run failure context\n' >&2
  printf 'run directories:\n' >&2
  if ((${#file_prefix[@]})); then
    "${file_prefix[@]}" find "$run_dir/vms" -maxdepth 2 -type f \( -name '*.json' -o -name '*.pid' \) -print 2>/dev/null >&2 || true
  else
    find "$run_dir/vms" -maxdepth 2 -type f \( -name '*.json' -o -name '*.pid' \) -print 2>/dev/null >&2 || true
  fi
  printf '\ncloud-hypervisor processes:\n' >&2
  ps -ef | grep '[c]loud-hypervisor' >&2 || true
  for vm_log_dir in "$log_dir"/vms/*; do
    [[ -d "$vm_log_dir" ]] || continue
    printf '\n--- %s console tail ---\n' "${vm_log_dir##*/}" >&2
    if ((${#file_prefix[@]})); then
      "${file_prefix[@]}" tail -n 80 "$vm_log_dir/console.log" 2>/dev/null >&2 || true
      printf '\n--- %s VMM stderr tail ---\n' "${vm_log_dir##*/}" >&2
      "${file_prefix[@]}" tail -n 80 "$vm_log_dir/cloud-hypervisor.stderr.log" 2>/dev/null >&2 || true
    else
      tail -n 80 "$vm_log_dir/console.log" 2>/dev/null >&2 || true
      printf '\n--- %s VMM stderr tail ---\n' "${vm_log_dir##*/}" >&2
      tail -n 80 "$vm_log_dir/cloud-hypervisor.stderr.log" 2>/dev/null >&2 || true
    fi
  done
}

vm_names=()
snapshot_names=()
cleanup_prefix_resources() {
  local refs ref
  refs=$(kb ps --json 2>/dev/null | jq -r '.[] | select((.name // "") | startswith("p6-bench-") or startswith("p6-concurrent-")) | .id' || true)
  while IFS= read -r ref; do
    [[ -n "$ref" ]] || continue
    kb delete "$ref" --force >/dev/null 2>&1 || true
  done <<<"$refs"

  refs=$(kb snapshot ls --json 2>/dev/null | jq -r '.[] | select((.name // "") | startswith("p6-bench-")) | .id' || true)
  while IFS= read -r ref; do
    [[ -n "$ref" ]] || continue
    kb snapshot rm "$ref" >/dev/null 2>&1 || true
  done <<<"$refs"
}

cleanup() {
  local ref
  cleanup_prefix_resources
  for ref in "${vm_names[@]:-}"; do kb delete "$ref" --force >/dev/null 2>&1 || true; done
  for ref in "${snapshot_names[@]:-}"; do kb snapshot rm "$ref" >/dev/null 2>&1 || true; done
}
trap 'cleanup' EXIT

run_iteration() {
  local index prefix
  index=$1
  prefix="p6-bench-${index}"
  local source="${prefix}-source" restored="${prefix}-restored" clone="${prefix}-clone"
  local stopped_snapshot="${prefix}-stopped" native_snapshot="${prefix}-native"
  local run_json source_id clone_json clone_id snapshot_json snapshot_id
  local start_ms end_ms exec_ms portable_restore_ms restart_ready_ms
  local source_run_ready source_run_total native_snapshot_ms native_pause_ms
  local clone_restore_ms clone_backend_ms clone_readiness_ms
  local vmm_ready_ms agent_ready_ms agent_overhead_ms
  local guest_overlay_ms guest_systemd_ms guest_agent_ms guest_multiuser_ms
  local guest_overlay_to_systemd_ms guest_systemd_to_agent_ms guest_agent_to_multiuser_ms
  local guest_overlay_to_multiuser_ms journal_log phase_log preserved_console_log preserved_journal_log
  local run_output

  vm_names+=("$source" "$restored" "$clone")
  snapshot_names+=("$stopped_snapshot" "$native_snapshot")
  step "iteration $index: cold run (timeout $run_timeout)"
  start_ms=$(now_ms)
  run_output=$(mktemp)
  if ! kb run "$image" --name "$source" --network "$network" --storage "$storage" --timeout "$run_timeout" >"$run_output" 2>&1; then
    printf 'cold run did not finish within %s or failed\n' "$run_timeout" >&2
    printf '%s\n' 'cold run output:' >&2
    cat "$run_output" >&2
    rm -f "$run_output"
    print_cold_run_context
    return 1
  fi
  run_json=$(cat "$run_output")
  rm -f "$run_output"
  end_ms=$(now_ms)
  source_id=$(jq -r '.id' <<<"$run_json")
  console_log=$(kb inspect "$source_id" --json | jq -r '.logDir + "/console.log"')
  preserved_console_log="$artifacts_dir/iteration-${index}-source-console.log"
  preserved_journal_log="$artifacts_dir/iteration-${index}-source-journal.log"
  preserve_console_log "$console_log" "$preserved_console_log"
  console_log="$preserved_console_log"
  source_run_ready=$(jq -r '.performance.readyDurationMs // 0' <<<"$run_json")
  vmm_ready_ms=$(jq -r '.performance.vmmAPIReadyDurationMs // 0' <<<"$run_json")
  agent_ready_ms=$(jq -r '.performance.agentReadyDurationMs // 0' <<<"$run_json")
  agent_overhead_ms=$((agent_ready_ms - vmm_ready_ms))
  phase_log=$(kb exec "$source_id" -- cat /run/kumabox/boot-phases 2>/dev/null || true)
  source_run_total=$((end_ms - start_ms))

  step "iteration $index: first exec"
  start_ms=$(now_ms)
  [[ $(kb exec "$source_id" -- true) == "" ]]
  end_ms=$(now_ms)
  exec_ms=$((end_ms - start_ms))

  journal_log=$(read_guest_startup_journal "$source_id")
  preserve_text "$phase_log" "$artifacts_dir/iteration-${index}-source-boot-phases.log"
  preserve_text "$journal_log" "$preserved_journal_log"
  guest_overlay_ms=$(phase_file_marker_ms "$phase_log" 'boot-phase=overlay-ready')
  guest_systemd_ms=$(journal_marker_ms "$journal_log" 'systemd 255.*running in system mode')
  guest_agent_ms=$(journal_marker_ms "$journal_log" 'Started kumabox-agent\.service')
  guest_multiuser_ms=$(journal_marker_ms "$journal_log" 'Reached target multi-user\.target')
  guest_overlay_to_systemd_ms=$(phase_duration_ms "$guest_overlay_ms" "$guest_systemd_ms")
  guest_systemd_to_agent_ms=$(phase_duration_ms "$guest_systemd_ms" "$guest_agent_ms")
  guest_agent_to_multiuser_ms=$(phase_duration_ms "$guest_agent_ms" "$guest_multiuser_ms")
  guest_overlay_to_multiuser_ms=$(phase_duration_ms "$guest_overlay_ms" "$guest_multiuser_ms")

  step "iteration $index: native snapshot and clone"
  snapshot_json=$(kb snapshot create "$source_id" --name "$native_snapshot" --type running)
  snapshot_id=$(jq -r '.id' <<<"$snapshot_json")
  snapshot_json=$(kb snapshot inspect "$snapshot_id" --json)
  native_snapshot_ms=$(jq -r '.performance.totalDurationMs // 0' <<<"$snapshot_json")
  native_pause_ms=$(jq -r '.performance.pauseDurationMs // 0' <<<"$snapshot_json")
  clone_json=$(kb clone "$snapshot_id" --name "$clone" --network "$network" --restore-mode copy)
  clone_id=$(jq -r '.id' <<<"$clone_json")
  clone_json=$(kb inspect "$clone_id" --json)
  clone_restore_ms=$(jq -r '.lastRestore.durationMs // 0' <<<"$clone_json")
  clone_backend_ms=$(jq -r '.lastRestore.backendRestoreDurationMs // 0' <<<"$clone_json")
  clone_readiness_ms=$(jq -r '.lastRestore.readinessDurationMs // 0' <<<"$clone_json")
  kb delete "$clone_id" --force >/dev/null

  step "iteration $index: stop, stopped snapshot, restore and start"
  kb stop "$source_id" >/dev/null
  start_ms=$(now_ms)
  snapshot_json=$(kb snapshot create "$source_id" --name "$stopped_snapshot")
  end_ms=$(now_ms)
  snapshot_id=$(jq -r '.id' <<<"$snapshot_json")
  snapshot_names+=("$snapshot_id")
  start_ms=$(now_ms)
  snapshot_json=$(kb snapshot restore "$snapshot_id" --name "$restored" --network "$network")
  end_ms=$(now_ms)
  portable_restore_ms=$((end_ms - start_ms))
  restored_id=$(jq -r '.id' <<<"$snapshot_json")
  start_ms=$(now_ms)
  run_json=$(kb start "$restored_id")
  end_ms=$(now_ms)
  restart_ready_ms=$(jq -r '.performance.readyDurationMs // 0' <<<"$run_json")
  [[ $restart_ready_ms -ge 0 ]]
  jq -n \
    --argjson iteration "$index" \
    --argjson runShellMs "$source_run_total" \
    --argjson runReadyMs "$source_run_ready" \
    --argjson vmmReadyMs "$vmm_ready_ms" \
    --argjson agentReadyMs "$agent_ready_ms" \
    --argjson agentOverheadMs "$agent_overhead_ms" \
    --argjson guestOverlayMs "$guest_overlay_ms" \
    --argjson guestSystemdMs "$guest_systemd_ms" \
    --argjson guestAgentMs "$guest_agent_ms" \
    --argjson guestMultiuserMs "$guest_multiuser_ms" \
    --argjson guestOverlayToSystemdMs "$guest_overlay_to_systemd_ms" \
    --argjson guestSystemdToAgentMs "$guest_systemd_to_agent_ms" \
    --argjson guestAgentToMultiuserMs "$guest_agent_to_multiuser_ms" \
    --argjson guestOverlayToMultiuserMs "$guest_overlay_to_multiuser_ms" \
    --arg journalLog "$preserved_journal_log" \
    --arg bootPhasesLog "$artifacts_dir/iteration-${index}-source-boot-phases.log" \
    --arg consoleLog "$preserved_console_log" \
    --argjson execMs "$exec_ms" \
    --argjson nativeSnapshotMs "$native_snapshot_ms" \
    --argjson nativePauseMs "$native_pause_ms" \
    --argjson cloneRestoreMs "$clone_restore_ms" \
    --argjson cloneBackendMs "$clone_backend_ms" \
    --argjson cloneReadinessMs "$clone_readiness_ms" \
    --argjson portableRestoreMs "$portable_restore_ms" \
    --argjson restartReadyMs "$restart_ready_ms" \
    '{iteration:$iteration,consoleLog:$consoleLog,journalLog:$journalLog,bootPhasesLog:$bootPhasesLog,runShellMs:$runShellMs,vmmReadyMs:$vmmReadyMs,agentReadyMs:$agentReadyMs,agentOverheadMs:$agentOverheadMs,guestOverlayMs:$guestOverlayMs,guestSystemdMs:$guestSystemdMs,guestAgentMs:$guestAgentMs,guestMultiuserMs:$guestMultiuserMs,guestOverlayToSystemdMs:$guestOverlayToSystemdMs,guestSystemdToAgentMs:$guestSystemdToAgentMs,guestAgentToMultiuserMs:$guestAgentToMultiuserMs,guestOverlayToMultiuserMs:$guestOverlayToMultiuserMs,runReadyMs:$runReadyMs,firstExecMs:$execMs,nativeSnapshotMs:$nativeSnapshotMs,nativePauseMs:$nativePauseMs,cloneRestoreMs:$cloneRestoreMs,cloneBackendMs:$cloneBackendMs,cloneReadinessMs:$cloneReadinessMs,portableRestoreMs:$portableRestoreMs,restartReadyMs:$restartReadyMs}'
}

run_concurrency_batch() {
  local batch_dir start_ms end_ms wall_ms name pid index
  local -a pids=()
  batch_dir=$(mktemp -d)
  start_ms=$(now_ms)
  for ((index = 1; index <= concurrency; index++)); do
    name="p6-concurrent-${index}"
    vm_names+=("$name")
    (kb run "$image" --name "$name" --network "$network" --storage "$storage" >"$batch_dir/$index.json") &
    pids+=("$!")
  done
  for pid in "${pids[@]}"; do wait "$pid"; done
  end_ms=$(now_ms)
  wall_ms=$((end_ms - start_ms))
  jq -s --argjson requested "$concurrency" --argjson wall "$wall_ms" '
    def percentile($values; $p):
      if ($values | length) == 0 then null
      else $values[(((($values | length) - 1) * $p) | floor)]
      end;
    [.[].performance.readyDurationMs // 0] | sort as $ready |
    {requested:$requested,wallMs:$wall,readyMs:$ready,
     readyP50Ms:percentile($ready; 0.50),readyP95Ms:percentile($ready; 0.95)}
  ' "$batch_dir"/*.json
  rm -rf "$batch_dir"
}

if [[ "$prepare_image" == true ]]; then
  step 'prepare current host binary, guest agent, and OCI image'
  build_project
  kb image rm "$image" >/dev/null 2>&1 || true
  kb image build "$image_ref" \
    --source daemon \
    --name "$image" \
    --platform linux/amd64 \
    --progress >/dev/null
fi

step "clean previous benchmark resources"
if ((${#file_prefix[@]})); then
  "${file_prefix[@]}" mkdir -p "$artifacts_dir"
  "${file_prefix[@]}" chmod a+rX "$artifacts_dir"
else
  mkdir -p "$artifacts_dir"
fi
vm_names=(p6-bench-1-source p6-bench-1-restored p6-bench-1-clone p6-bench-2-source p6-bench-2-restored p6-bench-2-clone p6-bench-3-source p6-bench-3-restored p6-bench-3-clone)
snapshot_names=(p6-bench-1-stopped p6-bench-1-native p6-bench-2-stopped p6-bench-2-native p6-bench-3-stopped p6-bench-3-native)
cleanup
vm_names=()
snapshot_names=()

samples_file=$(mktemp)
trap 'rm -f "$samples_file"; cleanup' EXIT
for ((i = 1; i <= iterations; i++)); do
  run_iteration "$i" >>"$samples_file"
done

concurrency_json='null'
if ((concurrency > 1)); then
  step "parallel run batch: $concurrency VMs"
  concurrency_json=$(run_concurrency_batch)
fi

cleanup

host_json=$(jq -n \
  --arg uname "$(uname -a)" \
  --arg cpu "$(nproc 2>/dev/null || echo unknown)" \
  --arg ch "$("$cloud_hypervisor" --version 2>&1 | head -n 1)" \
  --arg qemu "$("$qemu_img" --version 2>&1 | head -n 1)" \
  '{uname:$uname,cpuCount:$cpu,cloudHypervisor:$ch,qemuImg:$qemu}')
image_json=$(kb image inspect "$image" --json)
jq -s \
  --arg generatedAt "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --arg image "$image" \
  --arg network "$network" \
  --arg storage "$storage" \
  --arg artifacts "$artifacts_dir" \
  --argjson iterations "$iterations" \
  --argjson host "$host_json" \
  --argjson imageRecord "$image_json" \
  --argjson concurrency "$concurrency_json" \
  '
    def numbers($key): [.[].[$key] | select(type == "number")];
    def percentile($values; $p):
      if ($values | length) == 0 then null
      else $values[(((($values | length) - 1) * $p) | floor)]
      end;
    def metric($key):
      (numbers($key) | sort) as $values |
      {count:($values | length),p50:percentile($values; 0.50),p95:percentile($values; 0.95),max:($values | max)};
    . as $samples |
    {schema:"kumabox.p6.benchmark.v5",generatedAt:$generatedAt,image:$image,network:$network,storage:$storage,iterations:$iterations,artifactsDir:$artifacts,host:$host,imageRecord:$imageRecord,concurrency:$concurrency,samples:$samples,summary:{runShellMs:metric("runShellMs"),vmmReadyMs:metric("vmmReadyMs"),agentReadyMs:metric("agentReadyMs"),agentOverheadMs:metric("agentOverheadMs"),guestOverlayMs:metric("guestOverlayMs"),guestSystemdMs:metric("guestSystemdMs"),guestAgentMs:metric("guestAgentMs"),guestMultiuserMs:metric("guestMultiuserMs"),guestOverlayToSystemdMs:metric("guestOverlayToSystemdMs"),guestSystemdToAgentMs:metric("guestSystemdToAgentMs"),guestAgentToMultiuserMs:metric("guestAgentToMultiuserMs"),guestOverlayToMultiuserMs:metric("guestOverlayToMultiuserMs"),runReadyMs:metric("runReadyMs"),firstExecMs:metric("firstExecMs"),nativeSnapshotMs:metric("nativeSnapshotMs"),nativePauseMs:metric("nativePauseMs"),cloneRestoreMs:metric("cloneRestoreMs"),cloneBackendMs:metric("cloneBackendMs"),cloneReadinessMs:metric("cloneReadinessMs"),portableRestoreMs:metric("portableRestoreMs"),restartReadyMs:metric("restartReadyMs")}}
  ' "$samples_file" >"$output"

if [[ -n $baseline ]]; then
  jq -e --argjson p50 "$max_p50_regression" --argjson p95 "$max_p95_regression" \
    --slurpfile current "$output" --slurpfile previous "$baseline" '
      def metrics: ["runReadyMs","firstExecMs","nativeSnapshotMs","nativePauseMs","cloneRestoreMs","portableRestoreMs","restartReadyMs"];
      def limit($current; $old; $percent):
        if $old == 0 then true else $current <= ($old * (1 + ($percent / 100))) end;
      [metrics[] as $key |
        {metric:$key,
         currentP50:$current[0].summary[$key].p50,
         baselineP50:$previous[0].summary[$key].p50,
         currentP95:$current[0].summary[$key].p95,
         baselineP95:$previous[0].summary[$key].p95} |
        select((.currentP50 != null and .baselineP50 != null) and
          ((limit(.currentP50; .baselineP50; $p50) | not) or
           (limit(.currentP95; .baselineP95; $p95) | not)))] as $failures |
      if ($failures | length) == 0 then true
      else ($failures | stderr), false
      end
    ' >/dev/null || { echo "P6 benchmark regression gate failed" >&2; exit 1; }
fi

jq '{schema,generatedAt,image,network,iterations,concurrency,host,summary}' "$output"
echo "P6 benchmark written to $output" >&2
