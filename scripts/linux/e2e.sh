#!/usr/bin/env bash
set -Eeuo pipefail

# The only Linux end-to-end entry point. It deliberately uses the KumaBox
# system paths and validates the core OCI, agent, CNI, snapshot, and disk
# hotplug flows in one isolated run.

repo_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
kumabox="$repo_dir/bin/kumabox"
cloud_hypervisor=cloud-hypervisor
qemu_img=qemu-img
image=p6-agent-image
image_ref=kumabox/ubuntu:24.04-p6
network=cni:cocoon
storage=64M
metadata_backend=sqlite
go_bin=${GO_BIN:-}
keep=false

usage() {
  cat <<'EOF'
Usage: scripts/linux/e2e.sh [options]

Builds a Linux guest image and verifies OCI boot, agent exec, CNI cleanup,
stopped and native snapshots, restore/clone, and disk hotplug.

All runtime paths are fixed to KumaBox system defaults:
  /var/lib/kumabox, /run/kumabox, /var/log/kumabox

Options:
  --kumabox PATH
  --cloud-hypervisor PATH
  --qemu-img PATH
  --image NAME
  --image-ref REF
  --network NETWORK
  --storage SIZE
  --metadata-backend json|sqlite
  --go-bin PATH
  --keep                       preserve E2E VMs and snapshots after success
EOF
}

require_value() { [[ -n ${2:-} ]] || { echo "$1 requires a value" >&2; exit 2; }; }

while (($#)); do
  case "$1" in
    --kumabox) require_value "$1" "${2:-}"; kumabox=$2; shift 2 ;;
    --cloud-hypervisor) require_value "$1" "${2:-}"; cloud_hypervisor=$2; shift 2 ;;
    --qemu-img) require_value "$1" "${2:-}"; qemu_img=$2; shift 2 ;;
    --image) require_value "$1" "${2:-}"; image=$2; shift 2 ;;
    --image-ref) require_value "$1" "${2:-}"; image_ref=$2; shift 2 ;;
    --network) require_value "$1" "${2:-}"; network=$2; shift 2 ;;
    --storage) require_value "$1" "${2:-}"; storage=$2; shift 2 ;;
    --metadata-backend) require_value "$1" "${2:-}"; metadata_backend=$2; shift 2 ;;
    --go-bin) require_value "$1" "${2:-}"; go_bin=$2; shift 2 ;;
    --keep) keep=true; shift ;;
    -h|--help) usage; exit 0 ;;
    --root-dir|--run-dir|--log-dir|--metadata-path)
      echo "$1 is not supported; E2E uses KumaBox system defaults" >&2; exit 2 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ "$metadata_backend" == json || "$metadata_backend" == sqlite ]] || { echo "--metadata-backend must be json or sqlite" >&2; exit 2; }
[[ -x "$kumabox" ]] || { echo "kumabox is not executable: $kumabox" >&2; exit 1; }
command -v "$cloud_hypervisor" >/dev/null || { echo "cloud-hypervisor is required" >&2; exit 1; }
command -v "$qemu_img" >/dev/null || { echo "qemu-img is required" >&2; exit 1; }
command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }

if [[ $(id -u) -eq 0 ]]; then
  run=()
  build_user=${SUDO_USER:-}
  [[ -n "$build_user" && "$build_user" != root ]] || { echo "run this script with sudo from the development user" >&2; exit 1; }
else
  run=(sudo)
  build_user=$(id -un)
fi

step() { printf '\n==> %s\n' "$1"; }
kb() { "${run[@]}" "$kumabox" --cloud-hypervisor-bin "$cloud_hypervisor" --qemu-img-bin "$qemu_img" --metadata-backend "$metadata_backend" "$@"; }

names=(e2e-exec e2e-boot e2e-cni e2e-stopped-source e2e-stopped-restored e2e-native-source e2e-native-clone e2e-hotplug)
snapshots=(e2e-stopped e2e-stopped-import e2e-native)

cleanup() {
  local name snapshot
  for name in "${names[@]}"; do kb delete "$name" --force >/dev/null 2>&1 || true; done
  for snapshot in "${snapshots[@]}"; do kb snapshot rm "$snapshot" >/dev/null 2>&1 || true; done
  "${run[@]}" rm -f /var/lib/kumabox/e2e-hotplug.raw /var/lib/kumabox/e2e-stopped.kbsnap 2>/dev/null || true
}

failure_context() {
  local status=$?
  [[ $status -eq 0 ]] && return
  printf '\n==> E2E failure context\n' >&2
  kb ps --json 2>/dev/null >&2 || true
  find /var/log/kumabox/vms -maxdepth 2 -type f \( -name console.log -o -name cloud-hypervisor.stderr.log \) -print 2>/dev/null >&2 || true
}
trap failure_context EXIT

build_image() {
  step "build guest agent and OCI image"
  [[ -n "$go_bin" ]] || go_bin=$(command -v go || true)
  [[ -x "$go_bin" ]] || { echo "Go binary is required; pass --go-bin \$(go env GOROOT)/bin/go" >&2; exit 1; }
  "$go_bin" version | grep -Eq 'go1\.24\.[4-9]|go1\.(2[5-9]|[3-9][0-9])\.' || { echo "Go 1.24.4 or newer is required" >&2; exit 1; }
  command -v docker >/dev/null || { echo "docker is required to build $image_ref" >&2; exit 1; }
  local context="$repo_dir/oci-images/ubuntu" agent="$context/kumabox-agent-linux-amd64"
  sudo -u "$build_user" -H env GOOS=linux GOARCH=amd64 CGO_ENABLED=0 "$go_bin" build -o "$agent" "$repo_dir/cmd/agent"
  sudo -u "$build_user" -H docker build --platform linux/amd64 -f "$context/24.04/Dockerfile" -t "$image_ref" "$context"
  rm -f "$agent"
  kb image rm "$image" >/dev/null 2>&1 || true
  kb image build "$image_ref" --source daemon --name "$image" --platform linux/amd64 --json | jq .
}

wait_agent() { kb agent ping "$1" --timeout 90s >/dev/null; }
run_vm() { kb run "$image" --name "$1" --network "$2" --storage "$storage"; }

step "clean previous E2E resources"
cleanup
build_image

step "OCI boot and guest exec"
run_vm e2e-exec none | jq .
wait_agent e2e-exec
[[ $(kb exec e2e-exec -- uname -n) == e2e-exec ]]
[[ $(printf 'roundtrip' | kb exec e2e-exec -- cat) == roundtrip ]]
[[ $(kb exec --env FOO=bar e2e-exec -- sh -c 'printf %s "$FOO"') == bar ]]
if kb exec --user nobody e2e-exec -- true 2>&1 | grep -q USER_UNSUPPORTED; then :; else echo "guest user policy was not enforced" >&2; exit 1; fi
kb delete e2e-exec --force >/dev/null

step "OCI overlay boot"
run_vm e2e-boot "$network" | jq .
wait_agent e2e-boot
kb exec e2e-boot -- sh -c 'findmnt -n -o FSTYPE / | grep -qx overlay; findmnt -n -o OPTIONS / | grep -q lowerdir=' >/dev/null
kb delete e2e-boot --force >/dev/null

step "CNI allocation and DEL cleanup"
cni_json=$(run_vm e2e-cni "$network")
printf '%s\n' "$cni_json" | jq .
wait_agent e2e-cni
gateway=$(printf '%s' "$cni_json" | jq -r '.networkConfigs[0].network.gateway')
[[ -n "$gateway" && "$gateway" != null ]]
kb exec e2e-cni -- ping -c 1 -W 3 "$gateway" >/dev/null
kb delete e2e-cni --force >/dev/null
[[ $(kb network inspect e2e-cni --json | jq '.interfaces | length') == 0 ]]

step "stopped snapshot export import restore"
run_vm e2e-stopped-source none >/dev/null
wait_agent e2e-stopped-source
kb exec e2e-stopped-source -- sh -c 'printf stopped > /var/tmp/e2e-stopped; sync' >/dev/null
kb stop e2e-stopped-source --force >/dev/null
stopped_snapshot=$(kb snapshot create e2e-stopped-source --name e2e-stopped --json | jq -r .id)
kb snapshot export "$stopped_snapshot" --output /var/lib/kumabox/e2e-stopped.kbsnap --compression none >/dev/null
imported_snapshot=$(kb snapshot import /var/lib/kumabox/e2e-stopped.kbsnap --name e2e-stopped-import --json | jq -r .id)
kb snapshot restore "$imported_snapshot" --name e2e-stopped-restored --network none >/dev/null
wait_agent e2e-stopped-restored
[[ $(kb exec e2e-stopped-restored -- cat /var/tmp/e2e-stopped) == stopped ]]
kb delete e2e-stopped-source --force >/dev/null
kb delete e2e-stopped-restored --force >/dev/null

step "native snapshot clone"
run_vm e2e-native-source none >/dev/null
wait_agent e2e-native-source
kb exec e2e-native-source -- sh -c 'printf native > /var/tmp/e2e-native; sync' >/dev/null
native_snapshot=$(kb snapshot create e2e-native-source --name e2e-native --type running --json | jq -r .id)
kb clone "$native_snapshot" --name e2e-native-clone --network none --restore-mode copy >/dev/null
wait_agent e2e-native-clone
[[ $(kb exec e2e-native-clone -- cat /var/tmp/e2e-native) == native ]]
kb delete e2e-native-source --force >/dev/null
kb delete e2e-native-clone --force >/dev/null

step "disk hotplug"
run_vm e2e-hotplug "$network" >/dev/null
wait_agent e2e-hotplug
disk=/var/lib/kumabox/e2e-hotplug.raw
"${run[@]}" truncate -s 8M "$disk"
kb disk attach e2e-hotplug --path "$disk" --name e2e-data >/dev/null
kb device state e2e-hotplug | jq -e '.attachedDisks | any(.[]; .name == "e2e-data")' >/dev/null
kb disk detach e2e-hotplug --name e2e-data >/dev/null
kb device state e2e-hotplug | jq -e '(.attachedDisks // []) | length == 0' >/dev/null
kb network resize e2e-hotplug --nics 2 >/dev/null
kb network resize e2e-hotplug --nics 1 >/dev/null
kb delete e2e-hotplug --force >/dev/null

[[ "$keep" == true ]] || cleanup
trap - EXIT
printf '\nPASS: KumaBox Linux E2E completed\n'
