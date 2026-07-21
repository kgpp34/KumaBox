#!/usr/bin/env bash
set -euo pipefail

kumabox=./bin/kumabox
cloud_hypervisor=cloud-hypervisor
qemu_img=qemu-img
root_dir=/tmp/kumabox-p0/data
run_dir=/tmp/kumabox-p0/run
log_dir=/tmp/kumabox-p0/logs
image=p6-agent-image
network=cni:cocoon
storage=64M
cpus=1
memory=512M
max_concurrency=0
run_timeout=60s
output=/tmp/kumabox-p0/p6-density.json
use_sudo=false

usage() {
  cat <<'EOF'
Usage: scripts/linux/benchmark-density.sh [options]

Runs safe staged concurrency tests by reusing benchmark-p6.sh. It calculates a
conservative resource limit, tests 1, 2, 4... VMs, and stops at the first
failed stage. It requires an already-built binary and managed image.

  --kumabox PATH
  --cloud-hypervisor PATH
  --qemu-img PATH
  --root-dir PATH
  --run-dir PATH
  --log-dir PATH
  --image NAME
  --network NAME            defaults to cni:cocoon
  --storage SIZE            defaults to 64M
  --cpus N                  defaults to 1
  --memory SIZE             defaults to 512M
  --max-concurrency N       hard upper bound; 0 uses a safe calculated limit
  --run-timeout DURATION    defaults to 60s
  --output PATH             defaults to /tmp/kumabox-p0/p6-density.json
  --sudo
EOF
}

require_value() {
  [[ $# -ge 2 ]] || { echo "$1 requires a value" >&2; exit 2; }
  [[ -n "$2" ]] || { echo "$1 requires a value" >&2; exit 2; }
}

while (($#)); do
  case "$1" in
    --kumabox) require_value "$@"; kumabox=$2; shift 2 ;;
    --cloud-hypervisor) require_value "$@"; cloud_hypervisor=$2; shift 2 ;;
    --qemu-img) require_value "$@"; qemu_img=$2; shift 2 ;;
    --root-dir) require_value "$@"; root_dir=$2; shift 2 ;;
    --run-dir) require_value "$@"; run_dir=$2; shift 2 ;;
    --log-dir) require_value "$@"; log_dir=$2; shift 2 ;;
    --image) require_value "$@"; image=$2; shift 2 ;;
    --network) require_value "$@"; network=$2; shift 2 ;;
    --storage) require_value "$@"; storage=$2; shift 2 ;;
    --cpus) require_value "$@"; cpus=$2; shift 2 ;;
    --memory) require_value "$@"; memory=$2; shift 2 ;;
    --max-concurrency) require_value "$@"; max_concurrency=$2; shift 2 ;;
    --run-timeout) require_value "$@"; run_timeout=$2; shift 2 ;;
    --output) require_value "$@"; output=$2; shift 2 ;;
    --sudo) use_sudo=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ "$network" == cni:* ]] || { echo "density benchmark requires CNI networking" >&2; exit 2; }
[[ "$cpus" =~ ^[1-9][0-9]*$ && "$max_concurrency" =~ ^[0-9]+$ ]] || {
  echo "--cpus and --max-concurrency are invalid" >&2
  exit 2
}
[[ -x "$kumabox" ]] || { echo "kumabox is not executable: $kumabox" >&2; exit 1; }
for command in awk date df jq nproc sed "$cloud_hypervisor" "$qemu_img"; do
  command -v "$command" >/dev/null 2>&1 || { echo "required command not found: $command" >&2; exit 1; }
done

repo_dir="$(cd "$(dirname "$0")/../.." && pwd)"
benchmark="$repo_dir/scripts/linux/benchmark-p6.sh"
[[ -x "$benchmark" ]] || { echo "benchmark script is not executable: $benchmark" >&2; exit 1; }

if [[ "$use_sudo" == true ]]; then sudo -v; fi

size_bytes() {
  local value="$1" number unit multiplier
  number=$(printf '%s' "$value" | sed 's/[KMGTP].*$//')
  unit=$(printf '%s' "$value" | sed 's/^[0-9.]*//')
  case "$unit" in
    "") multiplier=1 ;;
    K|KB|KIB) multiplier=1024 ;;
    M|MB|MIB) multiplier=$((1024 * 1024)) ;;
    G|GB|GIB) multiplier=$((1024 * 1024 * 1024)) ;;
    T|TB|TIB) multiplier=$((1024 * 1024 * 1024 * 1024)) ;;
    *) return 1 ;;
  esac
  awk -v n="$number" -v m="$multiplier" 'BEGIN { printf "%.0f", n*m }'
}

memory_bytes=$(size_bytes "$memory")
storage_bytes=$(size_bytes "$storage")
available_memory=$(awk '/^MemAvailable:/ {print $2 * 1024; exit}' /proc/meminfo)
host_cpus=$(nproc)
per_vm_budget=$((memory_bytes + 256 * 1024 * 1024))
memory_limit=$((available_memory * 40 / 100 / per_vm_budget))
cpu_limit=$((host_cpus * 2 / cpus))
((cpu_limit < 1)) && cpu_limit=1
disk_available=$(df -Pk "$run_dir" | awk 'NR==2 {print $4 * 1024}')
disk_limit=$((disk_available / (storage_bytes * 2 + 128 * 1024 * 1024)))
((disk_limit < 1)) && disk_limit=1
safe_limit=$memory_limit
((cpu_limit < safe_limit)) && safe_limit=$cpu_limit
((disk_limit < safe_limit)) && safe_limit=$disk_limit
((safe_limit > 8)) && safe_limit=8
if ((max_concurrency > 0 && max_concurrency < safe_limit)); then safe_limit=$max_concurrency; fi
((safe_limit < 1)) && { echo "not enough available resources for one VM" >&2; exit 1; }

host_load=$(awk '{print $1}' /proc/loadavg)
preflight=$(jq -n \
  --argjson availableMemoryBytes "$available_memory" \
  --argjson hostCPUs "$host_cpus" \
  --argjson perVMBudgetBytes "$per_vm_budget" \
  --argjson memoryLimit "$memory_limit" \
  --argjson cpuLimit "$cpu_limit" \
  --argjson diskLimit "$disk_limit" \
  --argjson safeConcurrencyLimit "$safe_limit" \
  --arg loadAverage "$host_load" \
  '{availableMemoryBytes:$availableMemoryBytes,hostCPUs:$hostCPUs,perVMBudgetBytes:$perVMBudgetBytes,memoryLimit:$memoryLimit,cpuLimit:$cpuLimit,diskLimit:$diskLimit,safeConcurrencyLimit:$safeConcurrencyLimit,loadAverage:$loadAverage}')
echo "==> safe resource limit"
printf '%s\n' "$preflight"

run_benchmark() {
  if [[ "$use_sudo" == true ]]; then
    sudo "$benchmark" --kumabox "$kumabox" --cloud-hypervisor "$cloud_hypervisor" \
      --qemu-img "$qemu_img" --root-dir "$root_dir" --run-dir "$run_dir" \
      --log-dir "$log_dir" --image "$image" --network "$network" \
      --storage "$storage" --cpus "$cpus" --memory "$memory" --iterations 1 \
      --concurrency "$1" --run-timeout "$run_timeout" --skip-image-prepare \
      --output "$2" --sudo
  else
    "$benchmark" --kumabox "$kumabox" --cloud-hypervisor "$cloud_hypervisor" \
      --qemu-img "$qemu_img" --root-dir "$root_dir" --run-dir "$run_dir" \
      --log-dir "$log_dir" --image "$image" --network "$network" \
      --storage "$storage" --cpus "$cpus" --memory "$memory" --iterations 1 \
      --concurrency "$1" --run-timeout "$run_timeout" --skip-image-prepare \
      --output "$2"
  fi
}

concurrency=1
while ((concurrency <= safe_limit)); do
  batch_output="$output.concurrency-$concurrency.json"
  echo "==> test concurrency $concurrency/$safe_limit"
  if ! run_benchmark "$concurrency" "$batch_output"; then
    echo "concurrency $concurrency failed; higher stages were not attempted" >&2
    exit 1
  fi
  concurrency=$((concurrency * 2))
done

mkdir -p "$(dirname "$output")"
jq -n \
  --arg schema "kumabox.p6.density.v1" \
  --arg generatedAt "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --arg image "$image" --arg network "$network" --arg storage "$storage" \
  --arg memory "$memory" --argjson cpus "$cpus" \
  --argjson safeConcurrencyLimit "$safe_limit" \
  --argjson preflight "$preflight" \
  --argjson batches "$(jq -s '[.[] | {concurrency:.concurrency,host:.host,summary:.summary}]' "$output".concurrency-*.json)" \
  '{schema:$schema,generatedAt:$generatedAt,image:$image,network:$network,storage:$storage,cpus:$cpus,memory:$memory,safeConcurrencyLimit:$safeConcurrencyLimit,preflight:$preflight,batches:$batches}' \
  | tee "$output"

echo "density benchmark passed; results written to $output"
