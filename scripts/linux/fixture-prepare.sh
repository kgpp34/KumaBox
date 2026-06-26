#!/usr/bin/env bash
set -euo pipefail

mode="verify-direct"
fixture_dir="/tmp/kumabox-p0/fixtures"
root_disk=""
kernel=""
initrd=""
firmware=""

ubuntu_series="jammy"
fw_version="0.5.0"

usage() {
  cat <<'USAGE'
Usage: scripts/linux/fixture-prepare.sh --mode MODE [options]

Modes:
  verify-direct    Verify direct-boot fixtures for current P0-05 start path.
  download-uefi    Download UEFI/cloud-image fixtures for future cloud-image boot.
  verify-uefi      Verify downloaded UEFI/cloud-image fixtures.

Options:
  --fixture-dir PATH    Fixture directory, defaults to /tmp/kumabox-p0/fixtures
  --root-disk PATH      Root disk path for verify-direct
  --kernel PATH         Kernel path for verify-direct
  --initrd PATH         Initrd path for verify-direct
  --firmware PATH       Firmware path for verify-uefi
  --ubuntu-series NAME  Ubuntu cloud image series, defaults to jammy
  --fw-version VERSION  rust-hypervisor-firmware release, defaults to 0.5.0

Current KumaBox P0-05 uses direct boot:
  root disk + kernel + initrd

Cloud-image/UEFI boot uses:
  qcow2 cloud image + CLOUDHV.fd firmware
and is prepared here for the next implementation step.
USAGE
}

require_value() {
  local flag="$1"
  local value="${2:-}"
  if [[ -z "$value" ]]; then
    echo "$flag requires a value" >&2
    exit 2
  fi
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --mode)
      require_value "$1" "${2:-}"
      mode="$2"
      shift 2
      ;;
    --fixture-dir)
      require_value "$1" "${2:-}"
      fixture_dir="$2"
      shift 2
      ;;
    --root-disk)
      require_value "$1" "${2:-}"
      root_disk="$2"
      shift 2
      ;;
    --kernel)
      require_value "$1" "${2:-}"
      kernel="$2"
      shift 2
      ;;
    --initrd)
      require_value "$1" "${2:-}"
      initrd="$2"
      shift 2
      ;;
    --firmware)
      require_value "$1" "${2:-}"
      firmware="$2"
      shift 2
      ;;
    --ubuntu-series)
      require_value "$1" "${2:-}"
      ubuntu_series="$2"
      shift 2
      ;;
    --fw-version)
      require_value "$1" "${2:-}"
      fw_version="$2"
      shift 2
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

need_file() {
  local label="$1"
  local path="$2"
  if [[ -z "$path" ]]; then
    echo "$label path is required" >&2
    exit 2
  fi
  if [[ ! -f "$path" ]]; then
    echo "$label does not exist: $path" >&2
    exit 1
  fi
}

detect_download_arch() {
  local arch
  arch="$(uname -m)"
  case "$arch" in
    x86_64)
      ubuntu_arch="amd64"
      fw_suffix=""
      ;;
    aarch64|arm64)
      ubuntu_arch="arm64"
      fw_suffix="-aarch64"
      ;;
    *)
      echo "unsupported architecture: $arch" >&2
      exit 1
      ;;
  esac
}

download() {
  local url="$1"
  local dest="$2"
  local tmp="${dest}.tmp"

  if [[ -f "$dest" ]]; then
    echo "exists: $dest"
    return
  fi

  mkdir -p "$(dirname "$dest")"
  echo "download: $url"
  curl -fL --retry 3 --connect-timeout 20 -o "$tmp" "$url"
  mv "$tmp" "$dest"
}

write_manifest() {
  local path="$1"
  local root="$2"
  local fw="$3"
  cat >"$path" <<EOF
{
  "mode": "uefi",
  "rootDisk": "$root",
  "firmware": "$fw"
}
EOF
}

case "$mode" in
  verify-direct)
    need_file "root disk" "$root_disk"
    need_file "kernel" "$kernel"
    need_file "initrd" "$initrd"
    echo "direct boot fixtures verified"
    echo "root disk: $root_disk"
    echo "kernel:    $kernel"
    echo "initrd:    $initrd"
    ;;

  download-uefi)
    detect_download_arch
    mkdir -p "$fixture_dir"
    firmware="$fixture_dir/CLOUDHV.fd"
    root_disk="$fixture_dir/${ubuntu_series}-server-cloudimg-${ubuntu_arch}.img"

    fw_url="https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/${fw_version}/hypervisor-fw${fw_suffix}"
    image_url="https://cloud-images.ubuntu.com/${ubuntu_series}/current/${ubuntu_series}-server-cloudimg-${ubuntu_arch}.img"

    download "$fw_url" "$firmware"
    download "$image_url" "$root_disk"
    write_manifest "$fixture_dir/uefi-fixtures.json" "$root_disk" "$firmware"

    echo "UEFI/cloud-image fixtures downloaded"
    echo "root disk: $root_disk"
    echo "firmware:  $firmware"
    echo "manifest:  $fixture_dir/uefi-fixtures.json"
    ;;

  verify-uefi)
    detect_download_arch
    if [[ -z "$root_disk" ]]; then
      root_disk="$fixture_dir/${ubuntu_series}-server-cloudimg-${ubuntu_arch}.img"
    fi
    if [[ -z "$firmware" ]]; then
      firmware="$fixture_dir/CLOUDHV.fd"
    fi
    need_file "root disk" "$root_disk"
    need_file "firmware" "$firmware"
    echo "UEFI/cloud-image fixtures verified"
    echo "root disk: $root_disk"
    echo "firmware:  $firmware"
    ;;

  *)
    echo "unknown mode: $mode" >&2
    usage >&2
    exit 2
    ;;
esac
