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
image=p6-agent-image
image_ref=kumabox/ubuntu:24.04-p6
network=cni:cocoon
storage=64M
metadata_backend=sqlite
go_bin=${GO_BIN:-}
native_snapshot_memory=128M
keep=false
rebuild_image=false
fs_socket=
pci_bdf=
native_clone_id=
expected_agent_version=

usage() {
  cat <<'EOF'
Usage: scripts/linux/e2e.sh [options]

Builds a Linux guest image and verifies OCI boot, agent exec, CNI cleanup,
stopped and native snapshots, restore/clone, and disk hotplug.

All runtime paths are fixed to KumaBox system defaults:
  /var/lib/kumabox, /var/lib/kumabox/run, /var/log/kumabox

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

step() { printf '\n==> %s\n' "$1"; }
kb() { "${run[@]}" "$kumabox" --cloud-hypervisor-bin "$cloud_hypervisor" --qemu-img-bin "$qemu_img" --metadata-backend "$metadata_backend" "$@"; }
host() { "${run[@]}" "$@"; }

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
	printf '\n==> retained native clone record\n' >&2
	if [[ -n ${native_clone_id:-} ]]; then
		kb inspect "$native_clone_id" --json 2>&1 >&2 || true
	else
		kb inspect e2e-native-clone --json 2>&1 >&2 || true
	fi
	printf '\n==> retained native clone diagnostics\n' >&2
	find /var/lib/kumabox/diagnostics/native-clone -maxdepth 2 -type f -print -exec sh -c 'printf "\\n--- %s ---\\n" "$1"; tail -n 160 "$1"' _ {} \; 2>/dev/null >&2 || true
  find /var/log/kumabox/vms -maxdepth 2 -type f \( -name console.log -o -name cloud-hypervisor.stderr.log \) -print 2>/dev/null >&2 || true
  if [[ -n ${native_debug_dir:-} && -d "$native_debug_dir" ]]; then
    printf '\n==> preserved E2E diagnostics\n' >&2
    find "$native_debug_dir" -maxdepth 2 -type f -print 2>/dev/null >&2 || true
  fi
}
trap failure_context EXIT

capture_native_clone_debug() {
  local vm_json="$native_debug_dir/clone-vm.json"
  local api_socket run_dir log_dir netns_path tap pid console_path
  kb inspect e2e-native-clone --json >"$vm_json" 2>&1 || return 0
  native_clone_id=$(jq -r '.id // empty' "$vm_json")
  if [[ -n "$native_clone_id" ]]; then
    printf '%s\n' "$native_clone_id" >"$native_debug_dir/clone-vm-id"
  fi
  run_dir=$(jq -r '.runDir // empty' "$vm_json")
  log_dir=$(jq -r '.logDir // empty' "$vm_json")
  api_socket=$(jq -r '.apiSocket // empty' "$vm_json")
  netns_path=$(jq -r '.networkConfigs[0].netnsPath // empty' "$vm_json")
  tap=$(jq -r '.networkConfigs[0].tap // empty' "$vm_json")
  pid=$(jq -r '.pid // empty' "$vm_json")

  {
    printf 'captured_at=%s\n' "$(date -u +%FT%TZ)"
    printf 'run_dir=%s\nlog_dir=%s\napi_socket=%s\nnetns_path=%s\ntap=%s\npid=%s\n' "$run_dir" "$log_dir" "$api_socket" "$netns_path" "$tap" "$pid"
    df -h /var/lib/kumabox /var/log/kumabox
    findmnt -T /var/lib/kumabox/run -o TARGET,SOURCE,FSTYPE,OPTIONS
  } >"$native_debug_dir/clone-host-state.txt" 2>&1 || true

  if [[ -n "$run_dir" && -d "$run_dir" ]]; then
    host cp "$run_dir/cloud-hypervisor.json" "$native_debug_dir/cloud-hypervisor.json" 2>/dev/null || true
    host find -L "$run_dir" -maxdepth 3 -type f -printf '%n %s %p -> %l\n' | sort >"$native_debug_dir/run-files.txt" 2>&1 || true
    if [[ -d "$run_dir/.restore-staging/native" ]]; then
      host find -L "$run_dir/.restore-staging/native" -maxdepth 1 -type f -printf '%n %s %p -> %l\n' | sort >"$native_debug_dir/staged-memory.txt" 2>&1 || true
    fi
  fi
  if [[ -n "$pid" && "$pid" != 0 ]]; then
    host ps -fp "$pid" >"$native_debug_dir/vmm-process.txt" 2>&1 || true
    host sh -c 'tr "\\0" " " <"$1"' sh "/proc/$pid/cmdline" >"$native_debug_dir/vmm-command.txt" 2>&1 || true
  fi
  if [[ -n "$tap" ]]; then
    host ip -d link show "$tap" >"$native_debug_dir/tap-link.txt" 2>&1 || true
  fi
  if [[ -n "$netns_path" && -e "$netns_path" ]]; then
    host ip netns exec "$netns_path" ip -d link show >"$native_debug_dir/netns-links.txt" 2>&1 || true
    host ip netns exec "$netns_path" ip route show >"$native_debug_dir/netns-routes.txt" 2>&1 || true
  fi
  if [[ -n "$api_socket" && -S "$api_socket" ]]; then
    host curl --silent --show-error --unix-socket "$api_socket" http://localhost/api/v1/vm.info >"$native_debug_dir/vm-info.json" 2>"$native_debug_dir/vm-info.err" || true
    console_path=$(jq -r '.config.console.file // empty' "$native_debug_dir/vm-info.json" 2>/dev/null || true)
    if [[ -n "$console_path" && -r "$console_path" ]]; then
      host timeout 2s dd if="$console_path" iflag=nonblock status=none >"$native_debug_dir/console-pty.log" 2>"$native_debug_dir/console-pty.err" || true
    fi
  fi
  if [[ -n "$log_dir" && -d "$log_dir" ]]; then
    {
      for log in "$log_dir"/cloud-hypervisor.stderr.log "$log_dir"/cloud-hypervisor.stdout.log "$log_dir"/console.log; do
        [[ -f "$log" ]] || continue
        printf '\n--- %s ---\n' "$log"
        tail -n 240 "$log"
      done
    } >"$native_debug_dir/live-logs.txt" 2>&1 || true
  fi
}

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
  kb image build "$image_ref" --source daemon --name "$image" --platform linux/amd64 --json | jq .
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
  local name=$1 network_name=$2 memory=${3:-}
  local args=(run "$image" --name "$name" --network "$network_name" --storage "$storage")
  if [[ -n "$memory" ]]; then
    args+=(--memory "$memory")
  fi
  kb "${args[@]}"
}

step "build current host binary"
build_host_binary
resolve_agent_version

step "clean previous E2E resources"
cleanup
ensure_image

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
kb delete e2e-stopped-source --force >/dev/null
kb delete e2e-stopped-restored --force >/dev/null

step "native snapshot capture and clone"
# Native snapshots contain guest memory. Keep this scenario small so it is
# safe on hosts whose /run is a constrained tmpfs.
native_debug_dir=$(mktemp -d "${TMPDIR:-/tmp}/kumabox-e2e-native-clone.XXXXXX")
run_vm e2e-native-source "$network" "$native_snapshot_memory" >/dev/null
wait_agent e2e-native-source
kb exec e2e-native-source -- sh -c 'printf native > /var/tmp/e2e-native; sync' >/dev/null
kb inspect e2e-native-source --json >"$native_debug_dir/source-vm.json" 2>&1 || true
snapshot_output="$native_debug_dir/snapshot-create.out"
if ! kb snapshot create e2e-native-source --name e2e-native --type running >"$snapshot_output" 2>&1; then
  printf 'native snapshot capture failed:\n' >&2
  cat "$snapshot_output" >&2
  exit 1
fi
native_snapshot=$(jq -r .id "$snapshot_output")
kb snapshot inspect "$native_snapshot" --json >"$native_debug_dir/snapshot.json" 2>&1 || true
snapshot_data_dir=$(jq -r '.dataDir // empty' "$native_debug_dir/snapshot.json" 2>/dev/null || true)
if [[ -n "$snapshot_data_dir" ]]; then
  {
    printf 'snapshot_data_dir=%s\n' "$snapshot_data_dir"
    findmnt -T "$snapshot_data_dir" -o TARGET,SOURCE,FSTYPE,OPTIONS
    findmnt -T /var/lib/kumabox/run -o TARGET,SOURCE,FSTYPE,OPTIONS
    df -h "$snapshot_data_dir" /var/lib/kumabox/run
    find "$snapshot_data_dir/native" -maxdepth 1 -type f -printf '%n %s %p\n' | sort
  } >"$native_debug_dir/filesystem.txt" 2>&1 || true
fi

# Clone waits for guest readiness before it returns. Capture the transient VM
# while that wait is active because normal rollback removes its runtime and log
# directories after a failure.
clone_output="$native_debug_dir/clone.out"
kb clone "$native_snapshot" --name e2e-native-clone --network "$network" --restore-mode ondemand >"$clone_output" 2>&1 &
clone_pid=$!
clone_seen=false
for _ in $(seq 1 210); do
  if kb inspect e2e-native-clone --json >"$native_debug_dir/clone-vm.json" 2>/dev/null; then
    clone_seen=true
    capture_native_clone_debug || true
  fi
  if ! kill -0 "$clone_pid" 2>/dev/null; then
    break
  fi
  sleep 0.5
done
if ! wait "$clone_pid"; then
  clone_error=$(<"$clone_output")
  {
    printf 'clone_seen=%s\n' "$clone_seen"
    printf '\n--- filesystem ---\n'
    sed -n '1,200p' "$native_debug_dir/filesystem.txt"
    printf '\n--- staged memory ---\n'
    sed -n '1,200p' "$native_debug_dir/staged-memory.txt"
    printf '\n--- live logs ---\n'
    sed -n '1,300p' "$native_debug_dir/live-logs.txt"
	printf '\n--- VMM API state ---\n'
	sed -n '1,320p' "$native_debug_dir/vm-info.json"
	printf '\n--- VMM process ---\n'
	sed -n '1,120p' "$native_debug_dir/vmm-command.txt"
	printf '\n--- TAP and netns ---\n'
	sed -n '1,160p' "$native_debug_dir/tap-link.txt"
	sed -n '1,240p' "$native_debug_dir/netns-links.txt"
	printf '\n--- PTY console ---\n'
	sed -n '1,240p' "$native_debug_dir/console-pty.log"
  } >"$native_debug_dir/clone-debug.txt" 2>&1 || true
  printf 'native clone failed:\n%s\n' "$clone_error" >&2
  printf 'native clone diagnostics: %s\n' "$native_debug_dir" >&2
  sed -n '1,360p' "$native_debug_dir/clone-debug.txt" >&2 || true
  if [[ "$clone_error" != *RESTORE_MODE_UNSUPPORTED* ]]; then
    exit 1
  fi
  printf 'native clone: ondemand unavailable; falling back to %s copy restore\n' "$native_snapshot_memory"
  kb clone "$native_snapshot" --name e2e-native-clone --network "$network" --restore-mode copy >/dev/null
fi
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

[[ "$keep" == true ]] || cleanup
trap - EXIT
printf '\nPASS: KumaBox Linux E2E completed\n'
