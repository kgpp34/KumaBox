#!/usr/bin/env bash
set -euo pipefail

kumabox=./bin/kumabox
cloud_hypervisor=cloud-hypervisor
qemu_img=qemu-img
root_dir=/tmp/kumabox-p0/data
run_dir=/tmp/kumabox-p0/run
log_dir=/tmp/kumabox-p0/logs
image=ubuntu
source_name=p4-restore-source
restored_name=p4-restored
storage=64M
package=/tmp/kumabox-p0/p4-restore.kbsnap
use_sudo=false
reset_unowned_network=false
success=false

usage() {
  cat <<'EOF'
Usage: verify-stopped-snapshot-e2e.sh [options]

  --kumabox PATH
  --cloud-hypervisor PATH
  --qemu-img PATH
  --root-dir PATH
  --run-dir PATH
  --log-dir PATH
  --image REF
  --source-name NAME
  --name NAME
  --storage SIZE
  --package PATH
  --sudo
  --reset-unowned-network  remove an unowned kumabox0 after operator verification
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
    --name) restored_name=$2; shift 2 ;;
    --storage) storage=$2; shift 2 ;;
    --package) package=$2; shift 2 ;;
    --sudo) use_sudo=true; shift ;;
    --reset-unowned-network) reset_unowned_network=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

step() { printf '\n==> %s\n' "$1"; }
if [[ $use_sudo == true ]]; then
  kb_cmd=(sudo "$kumabox")
  rm_cmd=(sudo rm -f)
  ip_cmd=(sudo ip)
else
  kb_cmd=("$kumabox")
  rm_cmd=(rm -f)
  ip_cmd=(ip)
fi
kb() {
  "${kb_cmd[@]}" --root-dir "$root_dir" --run-dir "$run_dir" --log-dir "$log_dir" \
    --cloud-hypervisor-bin "$cloud_hypervisor" --qemu-img-bin "$qemu_img" "$@"
}
file_exists() {
  if [[ $use_sudo == true ]]; then
    sudo test -f "$1"
  else
    test -f "$1"
  fi
}

reset_unowned_test_bridge() {
  local bridge=kumabox0
  local owner_state=$root_dir/network/host-tap.json
  if file_exists "$owner_state" || ! "${ip_cmd[@]}" link show dev "$bridge" >/dev/null 2>&1; then
    return
  fi
  if [[ $reset_unowned_network != true ]]; then
    echo "unowned bridge $bridge exists; it may still serve VMs from another KumaBox root" >&2
    echo "inspect it first, then rerun with --reset-unowned-network only when it is safe to remove" >&2
    exit 1
  fi

  local slave
  local -a slaves=()
  for slave_path in /sys/class/net/"$bridge"/brif/*; do
    [[ -e $slave_path ]] || continue
    slave=${slave_path##*/}
    if [[ $slave != kbtap* ]]; then
      echo "refusing to reset unowned bridge $bridge with non-KumaBox slave $slave" >&2
      exit 1
    fi
    slaves+=("$slave")
  done

  echo "state: removing unowned leftover test bridge $bridge"
  for slave in "${slaves[@]}"; do
    "${ip_cmd[@]}" link delete "$slave"
  done
  "${ip_cmd[@]}" link delete "$bridge"
}

source_snapshot=${source_name}-disk
imported_snapshot=${source_name}-imported
source_id=
restored_id=

cleanup_named() {
  kb delete "$restored_name" --force >/dev/null 2>&1 || true
  kb delete "$source_name" --force >/dev/null 2>&1 || true
  kb snapshot rm "$imported_snapshot" >/dev/null 2>&1 || true
  kb snapshot rm "$source_snapshot" >/dev/null 2>&1 || true
}

on_exit() {
  if [[ $success == true ]]; then
    cleanup_named
    "${rm_cmd[@]}" "$package" >/dev/null 2>&1 || true
    return
  fi
  step "preserving failed restore state"
  echo "state: root_dir=$root_dir run_dir=$run_dir log_dir=$log_dir package=$package"
}
trap on_exit EXIT

for binary in jq sha256sum; do
  command -v "$binary" >/dev/null 2>&1 || { echo "$binary is required" >&2; exit 1; }
done

step "clean previous named P4 restore state"
cleanup_named
reset_unowned_test_bridge
"${rm_cmd[@]}" "$package" >/dev/null 2>&1 || true

step "run source VM with an independently allocated network identity"
source_json=$(kb run "$image" --name "$source_name" --network default --storage "$storage")
printf '%s\n' "$source_json" | jq .
source_id=$(jq -r '.id' <<<"$source_json")
source_disk=$(jq -r '[.storageConfigs[] | select(.role == "cow" or .role == "data")][0].path' <<<"$source_json")
source_mac=$(jq -r '.networkConfigs[0].mac // empty' <<<"$source_json")
source_ip=$(jq -r '.networkConfigs[0].network.ip // empty' <<<"$source_json")

step "stop source VM and capture portable writable state"
kb stop "$source_id" | jq .
snapshot_json=$(kb snapshot create "$source_id" --name "$source_snapshot")
printf '%s\n' "$snapshot_json" | jq .
snapshot_id=$(jq -r '.id' <<<"$snapshot_json")

step "export and import through the untrusted package boundary"
kb snapshot export "$snapshot_id" --output "$package" --compression none | jq .
ls -lh "$package"
import_json=$(kb snapshot import "$package" --name "$imported_snapshot")
printf '%s\n' "$import_json" | jq .
imported_id=$(jq -r '.id' <<<"$import_json")

step "restore a new CREATED VM with fresh storage and network identity"
restore_json=$(kb snapshot restore "$imported_id" --name "$restored_name" --network default)
printf '%s\n' "$restore_json" | jq .
restored_id=$(jq -r '.id' <<<"$restore_json")
restored_disk=$(jq -r '[.storageConfigs[] | select(.role == "cow" or .role == "data")][0].path' <<<"$restore_json")
restored_mac=$(jq -r '.networkConfigs[0].mac // empty' <<<"$restore_json")
restored_ip=$(jq -r '.networkConfigs[0].network.ip // empty' <<<"$restore_json")

[[ $(jq -r '.state' <<<"$restore_json") == created ]] || { echo "restored VM is not CREATED" >&2; exit 1; }
[[ $restored_id != "$source_id" ]] || { echo "restore reused source VM identity" >&2; exit 1; }
[[ $restored_disk != "$source_disk" ]] || { echo "restore reused source writable disk" >&2; exit 1; }
[[ $restored_mac != "$source_mac" ]] || { echo "restore reused source MAC" >&2; exit 1; }
[[ $restored_ip != "$source_ip" ]] || { echo "restore reused source IP" >&2; exit 1; }
printf 'state: source_vm=%s restored_vm=%s\n' "$source_id" "$restored_id"
printf 'state: source_disk=%s restored_disk=%s\n' "$source_disk" "$restored_disk"
printf 'state: source_mac=%s restored_mac=%s source_ip=%s restored_ip=%s\n' "$source_mac" "$restored_mac" "$source_ip" "$restored_ip"

base_family=$(jq -r '[.storageConfigs[] | select(.role == "cow")][0].base.family // empty' <<<"$restore_json")
if [[ $base_family == cloudimg ]]; then
  step "verify restored qcow2 points at the exact local base"
  expected_base=$(jq -r '[.storageConfigs[] | select(.role == "cow")][0].base.path' <<<"$restore_json")
  if [[ $use_sudo == true ]]; then
    backing=$(sudo "$qemu_img" info --output=json "$restored_disk" | jq -r '."full-backing-filename" // ."backing-filename"')
  else
    backing=$("$qemu_img" info --output=json "$restored_disk" | jq -r '."full-backing-filename" // ."backing-filename"')
  fi
  [[ $backing == "$expected_base" ]] || { echo "restored backing is $backing, want $expected_base" >&2; exit 1; }
  echo "pass: restored overlay backing=$backing"
fi

step "remove source VM and both snapshots before starting restored VM"
kb snapshot rm "$imported_id" | jq .
kb snapshot rm "$snapshot_id" | jq .
kb delete "$source_id" --force | jq .
source_id=
file_exists "$restored_disk" || { echo "restored writable disk disappeared with source resources" >&2; exit 1; }

step "start restored VM through the normal lifecycle"
started_json=$(kb start "$restored_id")
printf '%s\n' "$started_json" | jq .
[[ $(jq -r '.state' <<<"$started_json") == running ]] || { echo "restored VM did not start" >&2; exit 1; }

boot_mode=$(jq -r '.image.bootMode // empty' <<<"$started_json")
if [[ $boot_mode == direct ]]; then
  step "wait for restored OCI guest agent"
  kb agent ping "$restored_id" --timeout 120s | jq .
else
  step "wait for restored cloud guest network"
  for attempt in $(seq 1 48); do
    if ping -c 1 -W 1 "$restored_ip" >/dev/null 2>&1; then
      echo "pass: restored guest responds at $restored_ip"
      break
    fi
    if [[ $attempt -eq 48 ]]; then
      echo "restored guest did not respond at $restored_ip" >&2
      exit 1
    fi
    echo "state: ping attempt $attempt failed; waiting 5s"
    sleep 5
  done
fi

step "stop and delete restored VM"
kb stop "$restored_id" | jq .
kb delete "$restored_id" --force | jq .
restored_id=

success=true
echo "P4 stopped snapshot restore E2E verification passed"
