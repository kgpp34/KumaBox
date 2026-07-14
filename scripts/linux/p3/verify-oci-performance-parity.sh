#!/usr/bin/env bash
set -euo pipefail

overlay_path="oci-images/ubuntu/overlay.sh"

usage() {
  cat <<'USAGE'
Usage: scripts/linux/p3/verify-oci-performance-parity.sh [options]

Options:
  --overlay PATH  overlay initramfs hook path, defaults to oci-images/ubuntu/overlay.sh

Verifies the low-risk OCI boot performance guardrails that can be checked
without starting a VM. Full boot timing is reported by verify-oci-agent-transport.sh
and verify-oci-exec.sh through their metrics lines.
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
    --overlay) require_value "$1" "${2:-}"; overlay_path="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [[ ! -f "$overlay_path" ]]; then
  echo "overlay hook does not exist: $overlay_path" >&2
  exit 1
fi

if grep -q 'i.*-ge 1' "$overlay_path"; then
  echo "overlay hook still delays attach-order fallback by one second per disk" >&2
  exit 1
fi

if ! grep -q 'using attach-order fallback' "$overlay_path"; then
  echo "overlay hook no longer reports attach-order fallback diagnostics" >&2
  exit 1
fi

if ! grep -q 'udevadm settle' "$overlay_path"; then
  echo "overlay hook must settle udev before attach-order fallback" >&2
  exit 1
fi

echo "P3-11 OCI performance parity verification passed"
