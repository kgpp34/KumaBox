#!/usr/bin/env bash
set -Eeuo pipefail

# The only Linux end-to-end entry point. It deliberately uses the KumaBox
# system paths and validates the core OCI, agent, CNI, snapshot, and disk
# hotplug flows in one isolated run.

repo_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$repo_dir"
kumabox="$repo_dir/bin/kumabox"
cloud_hypervisor=cloud-hypervisor
qemu_img=qemu-img
image=kumabox-e2e
image_ref=kumabox/ubuntu:24.04-e2e
network=cni:kumabox
storage=64M
metadata_backend=sqlite
go_bin=${GO_BIN:-}
keep=false
rebuild_image=false
fs_socket=
pci_bdf=
e2e_phase=initialization
expected_agent_version=

usage() {
  cat <<'EOF'
Usage: test/e2e/e2e.sh [options]

Builds a Linux guest image and verifies image auto-detection, launch dry-run,
OCI boot, agent exec, CNI cleanup, package/directory/native snapshots,
restore/clone, and disk hotplug.

All runtime paths are fixed to KumaBox system defaults:
  /var/lib/kumabox, /var/lib/kumabox/run, /var/log/kumabox

Options:
  --kumabox PATH
  --cloud-hypervisor PATH
  --qemu-img PATH
  --image NAME                 managed image name, defaults to kumabox-e2e
  --image-ref REF              local OCI tag, defaults to kumabox/ubuntu:24.04-e2e
  --network NETWORK            VM network, defaults to cni:kumabox
  --storage SIZE
  --metadata-backend json|sqlite
  --go-bin PATH
  --rebuild-image             rebuild and re-import the managed OCI image
  --fs-socket PATH             verify virtio-fs attach/detach with this socket
  --pci BDF                    verify VFIO attach/detach with this host PCI device
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
    --rebuild-image) rebuild_image=true; shift ;;
    --fs-socket) require_value "$1" "${2:-}"; fs_socket=$2; shift 2 ;;
    --pci) require_value "$1" "${2:-}"; pci_bdf=$2; shift 2 ;;
    --keep) keep=true; shift ;;
    -h|--help) usage; exit 0 ;;
    --root-dir|--run-dir|--log-dir|--metadata-path)
      echo "$1 is not supported; E2E uses KumaBox system defaults" >&2; exit 2 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ "$metadata_backend" == json || "$metadata_backend" == sqlite ]] || { echo "--metadata-backend must be json or sqlite" >&2; exit 2; }
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

step() {
  e2e_phase=$1
  printf '\n==> %s\n' "$e2e_phase"
}
kb() { "${run[@]}" "$kumabox" --cloud-hypervisor-bin "$cloud_hypervisor" --qemu-img-bin "$qemu_img" --metadata-backend "$metadata_backend" "$@"; }

resolve_go_binary() {
  if [[ -z "$go_bin" ]]; then
    if [[ $(id -u) -eq 0 ]]; then
      go_bin=$(sudo -u "$build_user" -H sh -lc 'command -v go' 2>/dev/null || true)
    else
      go_bin=$(command -v go || true)
    fi
  fi
  [[ -x "$go_bin" ]] || {
    echo "Go binary is required; pass --go-bin \$(go env GOROOT)/bin/go" >&2
    exit 1
  }
  "$go_bin" version | grep -Eq 'go1\.24\.[4-9]|go1\.(2[5-9]|[3-9][0-9])\.' || {
    echo "Go 1.24.4 or newer is required: $("$go_bin" version)" >&2
    exit 1
  }
}

build_host_binary() {
  resolve_go_binary
  local commit build_time ldflags
  commit=$(git -C "$repo_dir" rev-parse --short HEAD)
  build_time=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  ldflags="-X github.com/kumabox/kumabox/internal/version.Version=0.0.0-dev -X github.com/kumabox/kumabox/internal/version.Commit=$commit -X github.com/kumabox/kumabox/internal/version.BuildTime=$build_time"
  mkdir -p "$(dirname "$kumabox")"
  if [[ $(id -u) -eq 0 ]]; then
    sudo -u "$build_user" -H "$go_bin" build -ldflags "$ldflags" -o "$kumabox" ./cmd/kumabox
  else
    "$go_bin" build -ldflags "$ldflags" -o "$kumabox" ./cmd/kumabox
  fi
  local binary_commit
  binary_commit=$("$kumabox" version --json | jq -r '.commit')
  [[ "$binary_commit" == "$commit" ]] || {
    echo "host binary commit mismatch: source=$commit binary=$binary_commit" >&2
    exit 1
  }
  printf 'host binary: commit=%s path=%s\n' "$binary_commit" "$kumabox"
}

resolve_agent_version() {
  if [[ $(id -u) -eq 0 ]]; then
    expected_agent_version=$(sudo -u "$build_user" -H "$go_bin" run ./cmd/agent version)
  else
    expected_agent_version=$("$go_bin" run ./cmd/agent version)
  fi
}

names=(e2e-exec e2e-boot e2e-cni e2e-stopped-source e2e-stopped-restored e2e-stopped-dir-restored e2e-native-source e2e-native-clone e2e-hotplug)
snapshots=(e2e-stopped e2e-stopped-import e2e-stopped-dir-import e2e-native)
temporary_images=(e2e-local-image)

cleanup() {
  local name snapshot temporary_image
  for name in "${names[@]}"; do kb delete "$name" --force >/dev/null 2>&1 || true; done
  for snapshot in "${snapshots[@]}"; do kb snapshot rm "$snapshot" >/dev/null 2>&1 || true; done
  for temporary_image in "${temporary_images[@]}"; do kb image rm "$temporary_image" >/dev/null 2>&1 || true; done
  "${run[@]}" rm -f /var/lib/kumabox/e2e-hotplug.raw /var/lib/kumabox/e2e-stopped.kbsnap \
    /var/lib/kumabox/e2e-local.qcow2 /var/lib/kumabox/e2e-firmware.fd \
    /var/lib/kumabox/e2e-metadata-backup.db /var/lib/kumabox/e2e-metadata-backup.db.backup.lock 2>/dev/null || true
  "${run[@]}" rm -rf /var/lib/kumabox/e2e-stopped-dir \
    /var/lib/kumabox/run/vms/kb_preview /var/lib/kumabox/storage/vms/kb_preview \
    /var/log/kumabox/vms/kb_preview 2>/dev/null || true
}

failure_context() {
  local status=$?
  [[ $status -eq 0 ]] && return
  printf '\n==> E2E failure context\n' >&2
  printf 'phase=%s\n' "$e2e_phase" >&2
  kb ps --json >&2 2>&1 || true
  printf '\n==> VM log files\n' >&2
  find /var/log/kumabox/vms -maxdepth 2 -type f \( -name console.log -o -name cloud-hypervisor.stderr.log \) -print 2>/dev/null >&2 || true
}
trap failure_context EXIT

build_image() {
  step "build guest agent and OCI image"
  resolve_go_binary
  command -v docker >/dev/null || { echo "docker is required to build $image_ref" >&2; exit 1; }
  local context="$repo_dir/oci-images/ubuntu"
  local agent="$context/kumabox-agent-linux-amd64"
  sudo -u "$build_user" -H env GOOS=linux GOARCH=amd64 CGO_ENABLED=0 "$go_bin" build -o "$agent" "$repo_dir/cmd/agent"
  sudo -u "$build_user" -H docker build --platform linux/amd64 --network=host -f "$context/24.04/Dockerfile" -t "$image_ref" "$context"
  rm -f "$agent"
  if kb image inspect "$image" --json >/dev/null 2>&1; then
    if ! kb image rm "$image" >/dev/null; then
      printf 'cannot replace managed image %q because it is still referenced; remove the listed VMs or rerun E2E after its cleanup succeeds:\n' "$image" >&2
      kb image inspect "$image" --json >&2 || true
      kb ps --json >&2 || true
      exit 1
    fi
  fi
  kb image add "$image_ref" --source daemon --name "$image" --platform linux/amd64 | jq .
}

ensure_image() {
  if [[ "$rebuild_image" == false ]] && kb image inspect "$image" --json >/dev/null 2>&1; then
    step "reuse managed OCI image: $image"
    return
  fi
  build_image
}

wait_agent() { kb agent ping "$1" --timeout 90s >/dev/null; }
run_vm() {
  local name=$1 network_name=$2
  kb run "$image" --name "$name" --network "$network_name" --storage "$storage"
}

step "build current host binary"
build_host_binary
resolve_agent_version

step "clean previous E2E resources"
cleanup
if [[ "$metadata_backend" == sqlite ]]; then
  if ! kb metadata status >/dev/null 2>&1; then
    step "initialize SQLite metadata"
    kb metadata init | jq .
  fi
fi
ensure_image

step "local image auto-detection"
"${run[@]}" "$qemu_img" create -q -f qcow2 /var/lib/kumabox/e2e-local.qcow2 8M
printf 'e2e firmware placeholder\n' | "${run[@]}" tee /var/lib/kumabox/e2e-firmware.fd >/dev/null
local_image=$(kb image add /var/lib/kumabox/e2e-local.qcow2 \
  --name e2e-local-image --firmware /var/lib/kumabox/e2e-firmware.fd \
  --qemu-img "$qemu_img")
printf '%s\n' "$local_image" | jq -e '
  .name == "e2e-local-image" and
  .source.type == "local-file" and
  .rootDisk.format == "qcow2" and
  .boot.mode == "uefi"
' >/dev/null
kb image rm e2e-local-image >/dev/null
"${run[@]}" rm -f /var/lib/kumabox/e2e-local.qcow2 /var/lib/kumabox/e2e-firmware.fd

step "launch plan dry-run"
vm_count_before=$(kb ps --json | jq 'length')
launch_plan=$(kb debug launch "$image" --storage "$storage" --json)
printf '%s\n' "$launch_plan" | jq -e '
  .schemaVersion == "kumabox.debug.launch.v1" and
  .dryRun == true and
  .vm.id == "kb_preview" and
  .vm.networks == ["none"] and
  (.launch.args | length > 0)
' >/dev/null
vm_count_after=$(kb ps --json | jq 'length')
[[ "$vm_count_before" == "$vm_count_after" ]] || {
  printf 'debug launch changed VM count: before=%s after=%s\n' "$vm_count_before" "$vm_count_after" >&2
  exit 1
}
for preview_path in \
  /var/lib/kumabox/run/vms/kb_preview \
  /var/lib/kumabox/storage/vms/kb_preview \
  /var/log/kumabox/vms/kb_preview; do
  if "${run[@]}" test -e "$preview_path"; then
    printf 'debug launch created preview path: %s\n' "$preview_path" >&2
    exit 1
  fi
done

step "OCI boot and guest exec"
run_vm e2e-exec none | jq .
agent_json=$(kb agent ping e2e-exec --timeout 90s)
actual_agent_version=$(printf '%s' "$agent_json" | jq -r '.agent.version // empty')
if [[ "$actual_agent_version" != "$expected_agent_version" ]]; then
  printf 'managed image %q contains kumabox-agent %q, but current source is %q; rerun once with --rebuild-image\n' \
    "$image" "$actual_agent_version" "$expected_agent_version" >&2
  exit 1
fi
[[ $(kb exec e2e-exec -- uname -n) == e2e-exec ]]
[[ $(printf 'roundtrip' | kb exec e2e-exec -- cat) == roundtrip ]]
[[ $(kb exec --env FOO=bar e2e-exec -- sh -c 'printf %s "$FOO"') == bar ]]
if unsupported_user_output=$(kb exec --user nobody e2e-exec -- true 2>&1); then
  printf 'guest user policy was not enforced: command unexpectedly succeeded\n' >&2
  exit 1
elif [[ "$unsupported_user_output" != *USER_UNSUPPORTED* ]]; then
  printf 'guest user policy returned an unexpected error:\n%s\n' "$unsupported_user_output" >&2
  exit 1
fi
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
stopped_snapshot=$(kb snapshot create e2e-stopped-source --name e2e-stopped | jq -r .id)
kb snapshot export "$stopped_snapshot" --output /var/lib/kumabox/e2e-stopped.kbsnap --compression none >/dev/null
imported_snapshot=$(kb snapshot import /var/lib/kumabox/e2e-stopped.kbsnap --name e2e-stopped-import | jq -r .id)
kb snapshot restore "$imported_snapshot" --name e2e-stopped-restored --network none >/dev/null
kb start e2e-stopped-restored >/dev/null
wait_agent e2e-stopped-restored
[[ $(kb exec e2e-stopped-restored -- cat /var/tmp/e2e-stopped) == stopped ]]
kb delete e2e-stopped-restored --force >/dev/null

step "stopped snapshot directory export import restore"
kb snapshot export "$stopped_snapshot" --to-dir /var/lib/kumabox/e2e-stopped-dir >/dev/null
directory_snapshot=$(kb snapshot import --from-dir /var/lib/kumabox/e2e-stopped-dir --name e2e-stopped-dir-import | jq -r .id)
kb snapshot restore "$directory_snapshot" --name e2e-stopped-dir-restored --network none >/dev/null
kb start e2e-stopped-dir-restored >/dev/null
wait_agent e2e-stopped-dir-restored
[[ $(kb exec e2e-stopped-dir-restored -- cat /var/tmp/e2e-stopped) == stopped ]]
kb delete e2e-stopped-source --force >/dev/null
kb delete e2e-stopped-dir-restored --force >/dev/null

step "native snapshot and clone"
step "native source start"
run_vm e2e-native-source "$network" >/dev/null
wait_agent e2e-native-source
kb exec e2e-native-source -- sh -c 'printf native > /var/tmp/e2e-native; sync' >/dev/null

step "native running snapshot capture"
native_snapshot=$(kb snapshot create e2e-native-source --name e2e-native --type running | jq -r .id)

step "native ondemand clone restore"
if ! clone_output=$(kb clone "$native_snapshot" --name e2e-native-clone --network "$network" --restore-mode ondemand 2>&1); then
  printf 'native clone failed:\n%s\n' "$clone_output" >&2
  if [[ "$clone_output" != *RESTORE_MODE_UNSUPPORTED* ]]; then
    exit 1
  fi
  printf 'native clone: ondemand unavailable; falling back to copy restore\n'
  kb clone "$native_snapshot" --name e2e-native-clone --network "$network" --restore-mode copy >/dev/null
fi
step "native clone post-return agent probe"
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
if [[ -n "$fs_socket" ]]; then
  [[ -S "$fs_socket" ]] || { echo "virtio-fs socket is not available: $fs_socket" >&2; exit 1; }
  kb fs attach e2e-hotplug --socket "$fs_socket" --tag e2e-share >/dev/null
  kb device state e2e-hotplug | jq -e '.attachedFilesystems | any(.[]; .tag == "e2e-share")' >/dev/null
  kb fs detach e2e-hotplug --tag e2e-share >/dev/null
fi
if [[ -n "$pci_bdf" ]]; then
  kb device attach e2e-hotplug --pci "$pci_bdf" --id e2e-pci >/dev/null
  kb device state e2e-hotplug | jq -e '.attachedPCIDevices | any(.[]; .id == "e2e-pci")' >/dev/null
  kb device detach e2e-hotplug --id e2e-pci >/dev/null
fi
kb delete e2e-hotplug --force >/dev/null

if [[ "$metadata_backend" == sqlite ]]; then
  step "SQLite metadata backup"
  kb metadata backup /var/lib/kumabox/e2e-metadata-backup.db | jq -e '.verified == true and .sizeBytes > 0' >/dev/null
fi

[[ "$keep" == true ]] || cleanup
trap - EXIT
printf '\nPASS: KumaBox Linux E2E completed\n'
