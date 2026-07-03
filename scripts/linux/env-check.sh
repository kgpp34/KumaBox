#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
cloud_hypervisor_path="cloud-hypervisor"
qemu_img_path="qemu-img"
fixture_dir=""
strict=0
json_output=0

usage() {
  cat <<'USAGE'
Usage: scripts/linux/env-check.sh [--kumabox PATH] [--cloud-hypervisor PATH] [--qemu-img PATH] [--fixture-dir PATH] [--strict] [--json]

Checks the minimum P0-01 environment and verifies kumabox version output.
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --kumabox)
      kumabox_path="${2:-}"
      if [[ -z "$kumabox_path" ]]; then
        echo "--kumabox requires a path" >&2
        exit 2
      fi
      shift 2
      ;;
    --fixture-dir)
      fixture_dir="${2:-}"
      if [[ -z "$fixture_dir" ]]; then
        echo "--fixture-dir requires a path" >&2
        exit 2
      fi
      shift 2
      ;;
    --cloud-hypervisor)
      cloud_hypervisor_path="${2:-}"
      if [[ -z "$cloud_hypervisor_path" ]]; then
        echo "--cloud-hypervisor requires a path" >&2
        exit 2
      fi
      shift 2
      ;;
    --qemu-img)
      qemu_img_path="${2:-}"
      if [[ -z "$qemu_img_path" ]]; then
        echo "--qemu-img requires a path" >&2
        exit 2
      fi
      shift 2
      ;;
    --strict)
      strict=1
      shift
      ;;
    --json)
      json_output=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

checks=()
failures=0

add_check() {
  local name="$1"
  local status="$2"
  local code="$3"
  local message="$4"
  checks+=("${name}|${status}|${code}|${message}")
  if [[ "$status" == "fail" ]]; then
    failures=$((failures + 1))
  fi
}

command_exists() {
  local cmd="$1"
  if [[ "$cmd" == */* ]]; then
    [[ -x "$cmd" ]]
  else
    command -v "$cmd" >/dev/null 2>&1
  fi
}

kvm_module_loaded() {
  lsmod 2>/dev/null | grep -Eq '^(kvm|kvm_intel|kvm_amd) '
}

wait_for_kvm_module() {
  local attempts=5
  local delay_seconds=1
  local i
  for ((i = 1; i <= attempts; i++)); do
    if kvm_module_loaded; then
      return 0
    fi
    sleep "$delay_seconds"
  done
  return 1
}

if [[ "$(uname -s)" != "Linux" ]]; then
  add_check "host" "fail" "NOT_LINUX" "env-check must run inside the Linux VM"
else
  add_check "host" "pass" "" "running on Linux"

  if [[ -e /dev/kvm ]]; then
    add_check "kvmDevice" "pass" "" "/dev/kvm exists"
  else
    add_check "kvmDevice" "fail" "KVM_DEVICE_MISSING" "/dev/kvm is missing"
  fi

  if [[ -r /dev/kvm && -w /dev/kvm ]]; then
    add_check "kvmPermission" "pass" "" "/dev/kvm is readable and writable"
  else
    add_check "kvmPermission" "fail" "KVM_PERMISSION_DENIED" "current user cannot read/write /dev/kvm"
  fi

  if grep -Eq '(^| )(vmx|svm)( |$)' /proc/cpuinfo 2>/dev/null; then
    add_check "cpuVirtualization" "pass" "" "cpu virtualization flag is present"
  else
    add_check "cpuVirtualization" "fail" "CPU_VIRT_FLAG_MISSING" "vmx/svm cpu flag is missing"
  fi

  if wait_for_kvm_module; then
    add_check "kvmModule" "pass" "" "kvm module is loaded"
  elif [[ -r /dev/kvm && -w /dev/kvm ]]; then
    add_check "kvmModule" "warn" "KVM_MODULE_NOT_LISTED" "kvm module is not listed by lsmod, but /dev/kvm is usable"
  else
    add_check "kvmModule" "fail" "KVM_MODULE_MISSING" "kvm kernel module is not loaded"
  fi
fi

if [[ ! -x "$kumabox_path" ]]; then
  add_check "kumabox" "fail" "KUMABOX_INVALID" "executable not found or not executable: $kumabox_path"
elif "$kumabox_path" version --json >/dev/null; then
  add_check "kumabox" "pass" "" "kumabox version --json succeeded"
else
  add_check "kumabox" "fail" "KUMABOX_INVALID" "kumabox version --json failed"
fi

if command_exists "$cloud_hypervisor_path"; then
  add_check "cloudHypervisor" "pass" "" "cloud-hypervisor is executable"
elif [[ "$strict" -eq 1 ]]; then
  add_check "cloudHypervisor" "fail" "CH_MISSING" "cloud-hypervisor is missing"
else
  add_check "cloudHypervisor" "warn" "CH_MISSING" "cloud-hypervisor is missing"
fi

if command_exists "$qemu_img_path"; then
  add_check "qemuImg" "pass" "" "qemu-img is executable"
elif [[ "$strict" -eq 1 ]]; then
  add_check "qemuImg" "fail" "QEMU_IMG_MISSING" "qemu-img is missing"
else
  add_check "qemuImg" "warn" "QEMU_IMG_MISSING" "qemu-img is missing"
fi

if [[ -n "$fixture_dir" ]]; then
  if [[ ! -d "$fixture_dir" ]]; then
    if [[ "$strict" -eq 1 ]]; then
      add_check "fixtures" "fail" "FIXTURE_MISSING" "fixture directory does not exist: $fixture_dir"
    else
      add_check "fixtures" "warn" "FIXTURE_MISSING" "fixture directory does not exist: $fixture_dir"
    fi
  else
    shopt -s nullglob
    root_disks=("$fixture_dir"/*.img "$fixture_dir"/*.qcow2 "$fixture_dir"/*.raw)
    firmwares=("$fixture_dir"/CLOUDHV.fd "$fixture_dir"/*.fd)
    kernels=("$fixture_dir"/vmlinuz* "$fixture_dir"/kernel*)
    initrds=("$fixture_dir"/initrd* "$fixture_dir"/*.initrd)
    shopt -u nullglob

    if [[ "${#root_disks[@]}" -gt 0 && ( "${#firmwares[@]}" -gt 0 || ( "${#kernels[@]}" -gt 0 && "${#initrds[@]}" -gt 0 ) ) ]]; then
      add_check "fixtures" "pass" "" "fixture directory contains bootable P0 assets"
    elif [[ "$strict" -eq 1 ]]; then
      add_check "fixtures" "fail" "FIXTURE_MISSING" "fixture directory is missing root disk plus firmware or kernel/initrd"
    else
      add_check "fixtures" "warn" "FIXTURE_MISSING" "fixture directory is missing root disk plus firmware or kernel/initrd"
    fi
  fi
fi

if [[ "$json_output" -eq 1 ]]; then
  overall="pass"
  if [[ "$failures" -gt 0 ]]; then
    overall="fail"
  fi

  printf '{"status":"%s","checks":[' "$overall"
  first=1
  for check in "${checks[@]}"; do
    IFS='|' read -r name status code message <<<"$check"
    if [[ "$first" -eq 0 ]]; then
      printf ','
    fi
    first=0
    printf '{"name":"%s","status":"%s","code":"%s","message":"%s"}' "$name" "$status" "$code" "$message"
  done
  printf ']}\n'
else
  for check in "${checks[@]}"; do
    IFS='|' read -r name status code message <<<"$check"
    if [[ -n "$code" ]]; then
      printf '%s: %s (%s): %s\n' "$status" "$name" "$code" "$message"
    else
      printf '%s: %s: %s\n' "$status" "$name" "$message"
    fi
  done
fi

if [[ "$failures" -gt 0 ]]; then
  exit 1
fi
