#!/usr/bin/env bash
set -Eeuo pipefail

kb=./bin/kumabox
ch=cloud-hypervisor
qemu=qemu-img
root=/tmp/kumabox-p0/data
run=/tmp/kumabox-p0/run
logs=/tmp/kumabox-p0/logs
image=p6-agent-image
network=cni:default
storage=64M
cpus=1
memory=512M
backend=json
metadata=
output=/tmp/kumabox-p0/p8-hotplug.json
use_sudo=false
cleanup=true
fs_socket=
pci=

usage() { printf '%s\n' 'Usage: verify-p8-hotplug.sh [options]' '  --kumabox PATH --cloud-hypervisor PATH --qemu-img PATH' '  --root-dir PATH --run-dir PATH --log-dir PATH' '  --image NAME --network NAME --storage SIZE --cpus N --memory SIZE' '  --metadata-backend json|sqlite --metadata-path PATH' '  --fs-socket PATH --pci BDF --output PATH --sudo --no-cleanup'; }
value() { [[ -n ${2:-} ]] || { echo "$1 requires a value" >&2; exit 2; }; }
while (($#)); do
  case "$1" in
    --kumabox) value "$1" "${2:-}"; kb=$2; shift 2;;
    --cloud-hypervisor) value "$1" "${2:-}"; ch=$2; shift 2;;
    --qemu-img) value "$1" "${2:-}"; qemu=$2; shift 2;;
    --root-dir) value "$1" "${2:-}"; root=$2; shift 2;;
    --run-dir) value "$1" "${2:-}"; run=$2; shift 2;;
    --log-dir) value "$1" "${2:-}"; logs=$2; shift 2;;
    --image) value "$1" "${2:-}"; image=$2; shift 2;;
    --network) value "$1" "${2:-}"; network=$2; shift 2;;
    --storage) value "$1" "${2:-}"; storage=$2; shift 2;;
    --cpus) value "$1" "${2:-}"; cpus=$2; shift 2;;
    --memory) value "$1" "${2:-}"; memory=$2; shift 2;;
    --metadata-backend) value "$1" "${2:-}"; backend=$2; shift 2;;
    --metadata-path) value "$1" "${2:-}"; metadata=$2; shift 2;;
    --fs-socket) value "$1" "${2:-}"; fs_socket=$2; shift 2;;
    --pci) value "$1" "${2:-}"; pci=$2; shift 2;;
    --output) value "$1" "${2:-}"; output=$2; shift 2;;
    --sudo) use_sudo=true; shift;;
    --no-cleanup) cleanup=false; shift;;
    -h|--help) usage; exit 0;;
    *) echo "unknown option: $1" >&2; usage >&2; exit 2;;
  esac
done

command -v jq >/dev/null || { echo 'jq is required' >&2; exit 1; }
[[ -x "$kb" ]] || { echo "kumabox is not executable: $kb" >&2; exit 1; }
command -v "$ch" >/dev/null || { echo 'cloud-hypervisor is required' >&2; exit 1; }
command -v "$qemu" >/dev/null || { echo 'qemu-img is required' >&2; exit 1; }
[[ "$backend" == json || "$backend" == sqlite ]] || { echo '--metadata-backend must be json or sqlite' >&2; exit 2; }
if [[ "$use_sudo" == true ]]; then sudo -v; p=(sudo); else p=(); fi

args=(--root-dir "$root" --run-dir "$run" --log-dir "$logs" --cloud-hypervisor-bin "$ch" --qemu-img-bin "$qemu" --metadata-backend "$backend")
[[ -n "$metadata" ]] && args+=(--metadata-path "$metadata")
vm=
disk=/tmp/kumabox-p8-hotplug-$$.raw
start=$(date +%s%3N)

finish() {
  local status=$?
  if [[ "$cleanup" == true && -n "$vm" ]]; then "${p[@]}" "$kb" "${args[@]}" delete "$vm" --force >/dev/null 2>&1 || true; fi
  "${p[@]}" rm -f "$disk" >/dev/null 2>&1 || true
  if ((status != 0)) && [[ -n "$vm" ]]; then "${p[@]}" "$kb" "${args[@]}" logs "$vm" --source all --tail 80 >&2 || true; fi
  exit "$status"
}
trap finish EXIT

echo '==> run VM'
run_args=(run "$image" --name p8-hotplug-e2e --network "$network" --storage "$storage" --cpus "$cpus" --memory "$memory")
[[ -n "$fs_socket" ]] && run_args+=(--shared-memory)
result=$("${p[@]}" "$kb" "${args[@]}" "${run_args[@]}")
vm=$(jq -r '.id' <<<"$result")
[[ -n "$vm" && "$vm" != null ]] || { echo 'VM id missing' >&2; exit 1; }

echo '==> refresh device state'
"${p[@]}" "$kb" "${args[@]}" device state "$vm" >/dev/null
echo '==> disk attach/detach'
"${p[@]}" dd if=/dev/zero of="$disk" bs=1M count=16 status=none
hotplug_start=$(date +%s%3N)
"${p[@]}" "$kb" "${args[@]}" disk attach "$vm" --path "$disk" --name p8data >/dev/null
"${p[@]}" "$kb" "${args[@]}" device state "$vm" | jq -e '.attachedDisks | any(.[]; .name == "p8data")' >/dev/null
"${p[@]}" "$kb" "${args[@]}" disk detach "$vm" --name p8data >/dev/null
"${p[@]}" "$kb" "${args[@]}" device state "$vm" | jq -e '(.attachedDisks // []) | length == 0' >/dev/null
hotplug_ms=$(( $(date +%s%3N) - hotplug_start ))

if [[ -n "$fs_socket" ]]; then
  echo '==> virtio-fs attach/detach'
  [[ -S "$fs_socket" ]] || { echo "not a Unix socket: $fs_socket" >&2; exit 1; }
  "${p[@]}" "$kb" "${args[@]}" fs attach "$vm" --socket "$fs_socket" --tag p8share >/dev/null
  "${p[@]}" "$kb" "${args[@]}" device state "$vm" | jq -e '.attachedFilesystems | any(.[]; .tag == "p8share")' >/dev/null
  "${p[@]}" "$kb" "${args[@]}" fs detach "$vm" --tag p8share >/dev/null
fi
if [[ "$network" != none ]]; then
  echo '==> network resize and restore'
  "${p[@]}" "$kb" "${args[@]}" network resize "$vm" --nics 2 >/dev/null
  "${p[@]}" "$kb" "${args[@]}" network resize "$vm" --nics 1 >/dev/null
fi
if [[ -n "$pci" ]]; then
  echo '==> VFIO attach/detach'
  "${p[@]}" "$kb" "${args[@]}" device attach "$vm" --pci "$pci" --id p8pci >/dev/null
  "${p[@]}" "$kb" "${args[@]}" device state "$vm" | jq -e '.attachedPCIDevices | any(.[]; .id == "p8pci")' >/dev/null
  "${p[@]}" "$kb" "${args[@]}" device detach "$vm" --id p8pci >/dev/null
fi

mkdir -p "$(dirname "$output")"
finish_ms=$(( $(date +%s%3N) - start ))
jq -n --arg schema kumabox.p8.hotplug.v1 --arg vmId "$vm" --arg network "$network" --argjson hotplugMs "$hotplug_ms" --argjson totalMs "$finish_ms" '{schema:$schema,vmId:$vmId,network:$network,rawDisk:{attachDetachMs:$hotplugMs},totalMs:$totalMs}' >"$output"
trap - EXIT
if [[ "$cleanup" == true ]]; then "${p[@]}" "$kb" "${args[@]}" delete "$vm" --force >/dev/null; "${p[@]}" rm -f "$disk"; fi
echo "PASS: P8 hotplug E2E completed; result=$output"
