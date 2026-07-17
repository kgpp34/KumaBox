#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd -- "$script_dir/../../.." && pwd)

kumabox=$repo_root/bin/kumabox
cocoon_source=${COCOON_SOURCE:-$HOME/Project/sandbox/cocoon}
cocoon_bin=
cloud_hypervisor=cloud-hypervisor
qemu_img=qemu-img
mkfs_erofs=mkfs.erofs
kumabox_image=p3-agent-image-v3
kumabox_ref=kumabox/ubuntu:24.04-p3
cocoon_image=ghcr.io/cocoonstack/cocoon/ubuntu:24.04
platform=linux/amd64
iterations=5
warmups=1
cpus=1
memory=512M
storage=64M
ready_timeout=180
prepare_images=true
build_binaries=true
use_sudo=false
state_dir=/tmp/kumabox-runtime-perf-state
result_dir=/tmp/kumabox-runtime-perf-$(date -u +%Y%m%dT%H%M%SZ)

usage() {
  cat <<'EOF'
Usage: scripts/linux/perf/compare-cocoon.sh [options]

Builds an isolated Cocoon binary from source, prepares both products' OCI
images, and compares Cloud Hypervisor lifecycle latency with identical VM
sizing and no networking.

  --kumabox PATH
  --cocoon-source DIR
  --cocoon-bin PATH          use an existing binary instead of build output
  --cloud-hypervisor PATH
  --qemu-img PATH
  --mkfs-erofs PATH
  --kumabox-image NAME
  --kumabox-ref REF
  --cocoon-image REF
  --platform OS/ARCH
  --iterations N             recorded iterations, defaults to 5
  --warmups N                unrecorded iterations per product, defaults to 1
  --cpus N                   defaults to 1
  --memory SIZE              defaults to 512M
  --storage SIZE             defaults to 64M
  --ready-timeout SECONDS    defaults to 180
  --state-dir DIR
  --result-dir DIR
  --no-prepare-images
  --no-build
  --sudo

Measured metrics:
  run_command_ms       CLI invocation through VMM start completion
  run_ready_ms         CLI invocation through guest-agent exec readiness
  snapshot_ms          durable crash-consistent native snapshot creation
  restore_command_ms   in-place native restore command completion
  restore_ready_ms     restore invocation through guest-agent exec readiness

Image download/build time is intentionally excluded. Raw samples, aggregate
statistics, speedup ratios, metadata, and both image manifests are written
under --result-dir.
EOF
}

require_value() {
  [[ -n ${2:-} ]] || { echo "$1 requires a value" >&2; exit 2; }
}

while (($#)); do
  case "$1" in
    --kumabox) require_value "$1" "${2:-}"; kumabox=$2; shift 2 ;;
    --cocoon-source) require_value "$1" "${2:-}"; cocoon_source=$2; shift 2 ;;
    --cocoon-bin) require_value "$1" "${2:-}"; cocoon_bin=$2; build_binaries=false; shift 2 ;;
    --cloud-hypervisor) require_value "$1" "${2:-}"; cloud_hypervisor=$2; shift 2 ;;
    --qemu-img) require_value "$1" "${2:-}"; qemu_img=$2; shift 2 ;;
    --mkfs-erofs) require_value "$1" "${2:-}"; mkfs_erofs=$2; shift 2 ;;
    --kumabox-image) require_value "$1" "${2:-}"; kumabox_image=$2; shift 2 ;;
    --kumabox-ref) require_value "$1" "${2:-}"; kumabox_ref=$2; shift 2 ;;
    --cocoon-image) require_value "$1" "${2:-}"; cocoon_image=$2; shift 2 ;;
    --platform) require_value "$1" "${2:-}"; platform=$2; shift 2 ;;
    --iterations) require_value "$1" "${2:-}"; iterations=$2; shift 2 ;;
    --warmups) require_value "$1" "${2:-}"; warmups=$2; shift 2 ;;
    --cpus) require_value "$1" "${2:-}"; cpus=$2; shift 2 ;;
    --memory) require_value "$1" "${2:-}"; memory=$2; shift 2 ;;
    --storage) require_value "$1" "${2:-}"; storage=$2; shift 2 ;;
    --ready-timeout) require_value "$1" "${2:-}"; ready_timeout=$2; shift 2 ;;
    --state-dir) require_value "$1" "${2:-}"; state_dir=$2; shift 2 ;;
    --result-dir) require_value "$1" "${2:-}"; result_dir=$2; shift 2 ;;
    --no-prepare-images) prepare_images=false; shift ;;
    --no-build) build_binaries=false; shift ;;
    --sudo) use_sudo=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ $iterations =~ ^[1-9][0-9]*$ ]] || { echo "--iterations must be positive" >&2; exit 2; }
[[ $warmups =~ ^[0-9]+$ ]] || { echo "--warmups must be non-negative" >&2; exit 2; }
[[ $cpus =~ ^[1-9][0-9]*$ ]] || { echo "--cpus must be positive" >&2; exit 2; }
[[ $ready_timeout =~ ^[1-9][0-9]*$ ]] || { echo "--ready-timeout must be positive" >&2; exit 2; }
[[ $(uname -s) == Linux ]] || { echo "benchmark must run on Linux/KVM" >&2; exit 1; }

for command in go jq "$cloud_hypervisor" "$qemu_img" "$mkfs_erofs"; do
  command -v "$command" >/dev/null 2>&1 || { echo "required command not found: $command" >&2; exit 1; }
done
[[ -e /dev/kvm ]] || { echo "/dev/kvm is missing" >&2; exit 1; }
[[ -d $cocoon_source ]] || { echo "Cocoon source directory not found: $cocoon_source" >&2; exit 1; }

mkdir -p "$result_dir" "$state_dir/bin"
result_dir=$(cd -- "$result_dir" && pwd)
state_dir=$(cd -- "$state_dir" && pwd)
samples=$result_dir/samples.csv
summary=$result_dir/summary.csv
comparison=$result_dir/comparison.csv
metadata=$result_dir/metadata.txt
attempt_log=$result_dir/readiness-attempts.log
: >"$attempt_log"
printf 'product,iteration,metric,duration_ms\n' >"$samples"

on_exit() {
  local status=$?
  if ((status == 0)); then
    return
  fi
  printf '\nbenchmark failed with status %s\n' "$status" >&2
  printf 'results: %s\nKumaBox state: %s\nCocoon state: %s\n' \
    "$result_dir" "$state_dir/kumabox" "$state_dir/cocoon" >&2
}
trap on_exit EXIT

if [[ $build_binaries == true ]]; then
  echo "==> build KumaBox"
  (cd -- "$repo_root" && make build)
  cocoon_bin=$state_dir/bin/cocoon
  echo "==> build Cocoon from $cocoon_source"
  (cd -- "$cocoon_source" && CGO_ENABLED=0 go build -o "$cocoon_bin" .)
elif [[ -z $cocoon_bin ]]; then
  cocoon_bin=$state_dir/bin/cocoon
fi
[[ -x $kumabox ]] || { echo "KumaBox binary is not executable: $kumabox" >&2; exit 1; }
[[ -x $cocoon_bin ]] || { echo "Cocoon binary is not executable: $cocoon_bin" >&2; exit 1; }

kb_root=$state_dir/kumabox/data
kb_run=$state_dir/kumabox/run
kb_log=$state_dir/kumabox/logs
cc_root=$state_dir/cocoon/data
cc_run=$state_dir/cocoon/run
cc_log=$state_dir/cocoon/logs

if [[ $use_sudo == true ]]; then
  sudo -v
  kb_prefix=(sudo)
  cc_prefix=(sudo env)
else
  kb_prefix=()
  cc_prefix=(env)
fi

kb() {
  "${kb_prefix[@]}" "$kumabox" \
    --root-dir "$kb_root" --run-dir "$kb_run" --log-dir "$kb_log" \
    --cloud-hypervisor-bin "$cloud_hypervisor" --qemu-img-bin "$qemu_img" "$@"
}

cc() {
  "${cc_prefix[@]}" \
    COCOON_ROOT_DIR="$cc_root" COCOON_RUN_DIR="$cc_run" COCOON_LOG_DIR="$cc_log" \
    COCOON_CH_BINARY="$cloud_hypervisor" "$cocoon_bin" "$@"
}

now_ns() {
  local value
  value=$(date +%s%N)
  [[ $value =~ ^[0-9]+$ ]] || { echo "GNU date with nanosecond support is required" >&2; exit 1; }
  printf '%s\n' "$value"
}

elapsed_ms() {
  printf '%s\n' "$((($2 - $1) / 1000000))"
}

record() {
  printf '%s,%s,%s,%s\n' "$1" "$2" "$3" "$4" >>"$samples"
}

wait_kumabox_ready() {
  local vm=$1 deadline=$((SECONDS + ready_timeout))
  until kb exec "$vm" -- true >/dev/null 2>>"$attempt_log"; do
    ((SECONDS < deadline)) || return 1
    sleep 0.1
  done
}

wait_cocoon_ready() {
  local vm=$1 deadline=$((SECONDS + ready_timeout))
  until cc vm exec "$vm" -- true >/dev/null 2>>"$attempt_log"; do
    ((SECONDS < deadline)) || return 1
    sleep 0.1
  done
}

prepare() {
  echo "==> environment"
  "$cloud_hypervisor" --version
  "$kumabox" version
  "$cocoon_bin" version

  mkdir -p "$kb_root" "$kb_run" "$kb_log" "$cc_root" "$cc_run" "$cc_log"
  if [[ $prepare_images == true ]]; then
    echo "==> prepare KumaBox image $kumabox_image"
    if ! kb image inspect "$kumabox_image" --json >/dev/null 2>&1; then
      kb image build "$kumabox_ref" --name "$kumabox_image" --platform "$platform" \
        --source auto --mkfs-erofs "$mkfs_erofs" --json >/dev/null
    fi
    echo "==> prepare Cocoon image $cocoon_image"
    if ! cc image inspect "$cocoon_image" >/dev/null 2>&1; then
      cc image pull "$cocoon_image"
    fi
  fi
  kb image inspect "$kumabox_image" --json >"$result_dir/kumabox-image.json"
  cc image inspect "$cocoon_image" >"$result_dir/cocoon-image.json"
}

benchmark_kumabox() {
  local iteration=$1 emit=$2 tag=$3
  local vm="perf-kb-${tag}-${iteration}" snap="perf-kb-snap-${tag}-${iteration}"
  local start end run_command run_ready snapshot_time restore_command restore_ready marker

  kb snapshot rm "$snap" >/dev/null 2>&1 || true
  kb delete "$vm" --force >/dev/null 2>&1 || true

  start=$(now_ns)
  kb run "$kumabox_image" --name "$vm" --cpus "$cpus" --memory "$memory" \
    --storage "$storage" --network none >"$result_dir/${vm}-run.json"
  end=$(now_ns)
  run_command=$(elapsed_ms "$start" "$end")
  wait_kumabox_ready "$vm" || { echo "KumaBox guest did not become ready: $vm" >&2; return 1; }
  end=$(now_ns)
  run_ready=$(elapsed_ms "$start" "$end")

  kb exec "$vm" -- sh -c 'printf snapshot > /var/tmp/runtime-perf-marker; sync' >/dev/null
  start=$(now_ns)
  kb snapshot create "$vm" --type running --consistent crash --name "$snap" \
    >"$result_dir/${vm}-snapshot.json"
  end=$(now_ns)
  snapshot_time=$(elapsed_ms "$start" "$end")
  kb exec "$vm" -- sh -c 'printf mutated > /var/tmp/runtime-perf-marker; sync' >/dev/null

  start=$(now_ns)
  kb restore "$vm" "$snap" --restore-mode copy >"$result_dir/${vm}-restore.json"
  end=$(now_ns)
  restore_command=$(elapsed_ms "$start" "$end")
  wait_kumabox_ready "$vm" || { echo "KumaBox restored guest did not become ready: $vm" >&2; return 1; }
  marker=$(kb exec "$vm" -- cat /var/tmp/runtime-perf-marker)
  [[ $marker == snapshot ]] || { echo "KumaBox restore correctness check failed: marker=$marker" >&2; return 1; }
  end=$(now_ns)
  restore_ready=$(elapsed_ms "$start" "$end")

  if [[ $emit == true ]]; then
    record kumabox "$iteration" run_command_ms "$run_command"
    record kumabox "$iteration" run_ready_ms "$run_ready"
    record kumabox "$iteration" snapshot_ms "$snapshot_time"
    record kumabox "$iteration" restore_command_ms "$restore_command"
    record kumabox "$iteration" restore_ready_ms "$restore_ready"
  fi
  printf 'kumabox iteration=%s run=%sms ready=%sms snapshot=%sms restore=%sms restore_ready=%sms\n' \
    "$iteration" "$run_command" "$run_ready" "$snapshot_time" "$restore_command" "$restore_ready"

  kb snapshot rm "$snap" >/dev/null
  kb delete "$vm" --force >/dev/null
}

benchmark_cocoon() {
  local iteration=$1 emit=$2 tag=$3
  local vm="perf-cc-${tag}-${iteration}" snap="perf-cc-snap-${tag}-${iteration}"
  local start end run_command run_ready snapshot_time restore_command restore_ready marker

  cc snapshot rm "$snap" >/dev/null 2>&1 || true
  cc vm rm --force "$vm" >/dev/null 2>&1 || true

  start=$(now_ns)
  cc vm run "$cocoon_image" --name "$vm" --cpu "$cpus" --memory "$memory" \
    --storage "$storage" --nics 0 -o json >"$result_dir/${vm}-run.json"
  end=$(now_ns)
  run_command=$(elapsed_ms "$start" "$end")
  wait_cocoon_ready "$vm" || { echo "Cocoon guest did not become ready: $vm" >&2; return 1; }
  end=$(now_ns)
  run_ready=$(elapsed_ms "$start" "$end")

  cc vm exec "$vm" -- sh -c 'printf snapshot > /var/tmp/runtime-perf-marker; sync' >/dev/null
  start=$(now_ns)
  cc snapshot save "$vm" --name "$snap" >/dev/null
  end=$(now_ns)
  snapshot_time=$(elapsed_ms "$start" "$end")
  cc vm exec "$vm" -- sh -c 'printf mutated > /var/tmp/runtime-perf-marker; sync' >/dev/null

  start=$(now_ns)
  cc vm restore "$vm" "$snap" --restore-mode copy -o json >"$result_dir/${vm}-restore.json"
  end=$(now_ns)
  restore_command=$(elapsed_ms "$start" "$end")
  wait_cocoon_ready "$vm" || { echo "Cocoon restored guest did not become ready: $vm" >&2; return 1; }
  marker=$(cc vm exec "$vm" -- cat /var/tmp/runtime-perf-marker)
  [[ $marker == snapshot ]] || { echo "Cocoon restore correctness check failed: marker=$marker" >&2; return 1; }
  end=$(now_ns)
  restore_ready=$(elapsed_ms "$start" "$end")

  if [[ $emit == true ]]; then
    record cocoon "$iteration" run_command_ms "$run_command"
    record cocoon "$iteration" run_ready_ms "$run_ready"
    record cocoon "$iteration" snapshot_ms "$snapshot_time"
    record cocoon "$iteration" restore_command_ms "$restore_command"
    record cocoon "$iteration" restore_ready_ms "$restore_ready"
  fi
  printf 'cocoon iteration=%s run=%sms ready=%sms snapshot=%sms restore=%sms restore_ready=%sms\n' \
    "$iteration" "$run_command" "$run_ready" "$snapshot_time" "$restore_command" "$restore_ready"

  cc snapshot rm "$snap" >/dev/null
  cc vm rm --force "$vm" >/dev/null
}

write_summary() {
  local product metric count min max mean median p95 index
  local -a values
  printf 'product,metric,count,min_ms,median_ms,p95_ms,max_ms,mean_ms\n' >"$summary"
  for product in kumabox cocoon; do
    for metric in run_command_ms run_ready_ms snapshot_ms restore_command_ms restore_ready_ms; do
      mapfile -t values < <(awk -F, -v p="$product" -v m="$metric" 'NR > 1 && $1 == p && $3 == m {print $4}' "$samples" | sort -n)
      count=${#values[@]}
      ((count > 0)) || continue
      min=${values[0]}
      max=${values[count-1]}
      mean=$(printf '%s\n' "${values[@]}" | awk '{sum += $1} END {printf "%.1f", sum / NR}')
      if ((count % 2 == 1)); then
        median=${values[count/2]}
      else
        median=$(awk -v a="${values[count/2-1]}" -v b="${values[count/2]}" 'BEGIN {printf "%.1f", (a+b)/2}')
      fi
      index=$(((95 * count + 99) / 100 - 1))
      p95=${values[index]}
      printf '%s,%s,%s,%s,%s,%s,%s,%s\n' \
        "$product" "$metric" "$count" "$min" "$median" "$p95" "$max" "$mean" >>"$summary"
    done
  done

  local kumabox_mean cocoon_mean ratio
  printf 'metric,kumabox_mean_ms,cocoon_mean_ms,kumabox_speedup\n' >"$comparison"
  for metric in run_command_ms run_ready_ms snapshot_ms restore_command_ms restore_ready_ms; do
    kumabox_mean=$(awk -F, -v m="$metric" '$1 == "kumabox" && $2 == m {print $8}' "$summary")
    cocoon_mean=$(awk -F, -v m="$metric" '$1 == "cocoon" && $2 == m {print $8}' "$summary")
    ratio=$(awk -v k="$kumabox_mean" -v c="$cocoon_mean" 'BEGIN {if (k == 0) print "n/a"; else printf "%.3fx", c/k}')
    printf '%s,%s,%s,%s\n' "$metric" "$kumabox_mean" "$cocoon_mean" "$ratio" >>"$comparison"
  done
}

prepare
{
  printf 'timestamp=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf 'host=%s\n' "$(uname -a)"
  printf 'cloud_hypervisor=%s\n' "$($cloud_hypervisor --version 2>&1 | head -n 1)"
  printf 'kumabox_commit=%s\n' "$(git -C "$repo_root" rev-parse HEAD)"
  printf 'cocoon_commit=%s\n' "$(git -C "$cocoon_source" rev-parse HEAD)"
  printf 'kumabox_image=%s\nkumabox_ref=%s\n' "$kumabox_image" "$kumabox_ref"
  printf 'cocoon_image=%s\n' "$cocoon_image"
  printf 'backend=cloud-hypervisor\ncpus=%s\nmemory=%s\nstorage=%s\nnetwork=none\n' "$cpus" "$memory" "$storage"
  printf 'snapshot_consistency=crash\nrestore_mode=copy\niterations=%s\nwarmups=%s\n' "$iterations" "$warmups"
} >"$metadata"

for ((i = 1; i <= warmups; i++)); do
  echo "==> warmup $i/$warmups"
  benchmark_kumabox "$i" false warmup
  benchmark_cocoon "$i" false warmup
done

for ((i = 1; i <= iterations; i++)); do
  echo "==> measured iteration $i/$iterations"
  if ((i % 2 == 1)); then
    benchmark_kumabox "$i" true measured
    benchmark_cocoon "$i" true measured
  else
    benchmark_cocoon "$i" true measured
    benchmark_kumabox "$i" true measured
  fi
done

write_summary
echo
column -s, -t "$summary" 2>/dev/null || cat "$summary"
echo
echo "KumaBox speedup is Cocoon mean / KumaBox mean; above 1.0x means KumaBox is faster."
column -s, -t "$comparison" 2>/dev/null || cat "$comparison"
printf '\nresults: %s\n' "$result_dir"
