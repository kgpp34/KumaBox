#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"

usage() {
  cat <<'USAGE'
Usage:
  scripts/linux/verify.sh CHECK [args...]
  scripts/linux/verify.sh PHASE CHECK [args...]

Checks:
  p0 start | reconcile | stop | logs | delete | gc
  p1 image | image-ref | image-rm-gc | cidata-firstboot
  p2 config | allocator | hosttap | render | inspect | e2e | cleanup | gc | cni | multinic | queues | parity
  p3 base-image | resolver | content-store | erofs-builder | boot-profile | cow-runtime | direct-boot-network | metadata | agent | exec | gc | performance

Examples:
  scripts/linux/verify.sh p2 e2e --root-disk /tmp/kumabox-p0/fixtures/jammy-server-cloudimg-amd64.img --firmware /tmp/kumabox-p0/fixtures/CLOUDHV.fd --sudo
  scripts/linux/verify.sh p3 direct-boot-network --kumabox ./bin/kumabox --sudo

Direct script paths are also available under scripts/linux/p0, p1, p2, and p3.
USAGE
}

die() {
  echo "$*" >&2
  usage >&2
  exit 2
}

if [[ $# -lt 1 ]]; then
  usage
  exit 0
fi

case "$1" in
  -h|--help)
    usage
    exit 0
    ;;
esac

phase=""
check="$1"
shift

case "$check" in
  p0|p1|p2|p3)
    phase="$check"
    [[ $# -gt 0 ]] || die "missing check name for $phase"
    check="$1"
    shift
    ;;
  p0-*|p1-*|p2-*|p3-*)
    phase="${check%%-*}"
    check="${check#*-}"
    ;;
  *)
    die "unknown check: $check"
    ;;
esac

target=""
case "$phase:$check" in
  p0:start) target="$script_dir/p0/verify-start.sh" ;;
  p0:reconcile) target="$script_dir/p0/verify-reconcile.sh" ;;
  p0:stop) target="$script_dir/p0/verify-stop.sh" ;;
  p0:logs) target="$script_dir/p0/verify-logs.sh" ;;
  p0:delete) target="$script_dir/p0/verify-delete.sh" ;;
  p0:gc) target="$script_dir/p0/verify-gc.sh" ;;

  p1:image) target="$script_dir/p1/verify-image.sh" ;;
  p1:image-ref) target="$script_dir/p1/verify-image-ref.sh" ;;
  p1:image-rm-gc) target="$script_dir/p1/verify-image-rm-gc.sh" ;;
  p1:cidata-firstboot) target="$script_dir/p1/verify-cidata-firstboot.sh" ;;

  p2:config) target="$script_dir/p2/verify-network-config.sh" ;;
  p2:allocator) target="$script_dir/p2/verify-network-allocator.sh" ;;
  p2:hosttap|p2:host-tap) target="$script_dir/p2/verify-hosttap-network.sh" ;;
  p2:render) target="$script_dir/p2/verify-network-render.sh" ;;
  p2:inspect) target="$script_dir/p2/verify-network-inspect.sh" ;;
  p2:e2e) target="$script_dir/p2/verify-network-e2e.sh" ;;
  p2:cleanup) target="$script_dir/p2/verify-network-cleanup.sh" ;;
  p2:gc) target="$script_dir/p2/verify-network-gc.sh" ;;
  p2:cni) target="$script_dir/p2/verify-cni-provider.sh" ;;
  p2:multinic|p2:multi-nic) target="$script_dir/p2/verify-multinic-model.sh" ;;
  p2:cni-multinic) target="$script_dir/p2/verify-multinic-model.sh"; set -- --mode cni "$@" ;;
  p2:hosttap-multinic|p2:host-tap-multinic) target="$script_dir/p2/verify-multinic-model.sh"; set -- --mode host-tap "$@" ;;
  p2:queues) target="$script_dir/p2/verify-network-queues.sh" ;;
  p2:parity) target="$script_dir/p2/verify-network-parity.sh" ;;

  p3:base-image) target="$script_dir/p3/verify-oci-base-image.sh" ;;
  p3:resolver) target="$script_dir/p3/verify-oci-resolver.sh" ;;
  p3:content-store) target="$script_dir/p3/verify-oci-content-store.sh" ;;
  p3:erofs-builder|p3:erofs) target="$script_dir/p3/verify-oci-erofs-builder.sh" ;;
  p3:boot-profile) target="$script_dir/p3/verify-oci-boot-profile.sh" ;;
  p3:cow-runtime|p3:cow) target="$script_dir/p3/verify-oci-cow-runtime.sh" ;;
  p3:direct-boot-network|p3:network) target="$script_dir/p3/verify-oci-direct-boot-network.sh" ;;
  p3:metadata) target="$script_dir/p3/verify-oci-metadata.sh" ;;
  p3:agent|p3:agent-transport) target="$script_dir/p3/verify-oci-agent-transport.sh" ;;
  p3:exec) target="$script_dir/p3/verify-oci-exec.sh" ;;
  p3:gc) target="$script_dir/p3/verify-oci-gc.sh" ;;
  p3:performance|p3:performance-parity) target="$script_dir/p3/verify-oci-performance-parity.sh" ;;

  *) die "unknown check: $phase $check" ;;
esac

exec "$target" "$@"
