#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd -- "$script_dir/../.." && pwd)

usage() {
  cat <<'EOF'
Usage: scripts/linux/verify.sh unit|cni|oci|snapshot|all [options]

  unit      run deterministic Go tests
  cni       run the real bridge + host-local CNI datapath E2E
  oci       build, boot, ping, and exec through a managed OCI microVM
  snapshot  run stopped, native, fs-consistent, and hibernate E2E scenarios
  all       run all four suites in dependency order

Options after the suite name are forwarded to its script. The all suite accepts
only options shared by all E2E scripts, such as --kumabox, --root-dir,
--run-dir, --log-dir, --cloud-hypervisor, --qemu-img, --storage, and --sudo.
EOF
}

[[ $# -gt 0 ]] || { usage; exit 0; }
suite=$1
shift

run_unit() {
  (cd -- "$repo_root" && go test ./... -count=1)
}

case $suite in
  unit) [[ $# -eq 0 ]] || { echo "unit does not accept options" >&2; exit 2; }; run_unit ;;
  cni) exec "$script_dir/verify-cni.sh" "$@" ;;
  oci) exec "$script_dir/verify-oci.sh" "$@" ;;
  snapshot) exec "$script_dir/verify-snapshot.sh" "$@" ;;
  all)
    run_unit
    "$script_dir/verify-oci.sh" "$@"
    "$script_dir/verify-cni.sh" "$@"
    "$script_dir/verify-snapshot.sh" "$@"
    ;;
  -h|--help) usage ;;
  *) echo "unknown verification suite: $suite" >&2; usage >&2; exit 2 ;;
esac
