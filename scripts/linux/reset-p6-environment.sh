#!/usr/bin/env bash
set -Eeuo pipefail

purge_cocoon_oci=false
confirmed=false

usage() {
  cat <<'EOF'
Usage: scripts/linux/reset-p6-environment.sh --yes [--purge-cocoon-oci]

Stop benchmark/KumaBox/Cocoon processes and remove their runtime state.

Default cleanup removes VM data, logs, CNI state, snapshot state, TAP devices,
bridges, and network namespaces. OCI image data is preserved by default.

  --yes                confirm destructive cleanup
  --purge-cocoon-oci  also remove /var/lib/cocoon/oci
EOF
}

while (($#)); do
  case "$1" in
    --yes) confirmed=true; shift ;;
    --purge-cocoon-oci) purge_cocoon_oci=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) printf 'unknown argument: %s\n' "$1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ "$confirmed" == true ]] || {
  printf '%s\n' 'This removes VM/runtime data and stops Cocoon and KumaBox processes.' >&2
  printf '%s\n' 'Re-run with --yes to continue.' >&2
  exit 2
}

if [[ $(id -u) -ne 0 ]]; then
  exec sudo -- "$0" "$@" --yes
fi

printf '%s\n' '==> stop benchmark and VM processes'
pkill -TERM -f 'scripts/linux/benchmark-p6.sh' 2>/dev/null || true
pkill -TERM -f '/tmp/kumabox-p0/run/vms/.*/ch.sock' 2>/dev/null || true
pkill -TERM -f '/var/lib/cocoon/run/cloudhypervisor' 2>/dev/null || true
pkill -TERM -x cocoon 2>/dev/null || true
sleep 3
pkill -KILL -f 'scripts/linux/benchmark-p6.sh' 2>/dev/null || true
pkill -KILL -f '/tmp/kumabox-p0/run/vms/.*/ch.sock' 2>/dev/null || true
pkill -KILL -f '/var/lib/cocoon/run/cloudhypervisor' 2>/dev/null || true
pkill -KILL -x cocoon 2>/dev/null || true

printf '%s\n' '==> remove KumaBox runtime data but preserve OCI image store'
rm -rf \
  /tmp/kumabox-p0/run \
  /tmp/kumabox-p0/logs \
  /tmp/kumabox-p0/p6-baseline.json \
  /tmp/kumabox-p0/p6-baseline.json.logs \
  /tmp/kumabox-p0/manual-run-diagnostic

printf '%s\n' '==> remove Cocoon runtime state'
rm -rf \
  /var/lib/cocoon/run \
  /var/lib/cocoon/cloudhypervisor/db \
  /var/lib/cocoon/cni/db \
  /var/lib/cocoon/cni/cache \
  /var/lib/cocoon/snapshot/db \
  /var/lib/cocoon/snapshot/localfile \
  /var/log/cocoon

if [[ "$purge_cocoon_oci" == true ]]; then
  printf '%s\n' '==> remove Cocoon OCI image data'
  rm -rf /var/lib/cocoon/oci
fi

printf '%s\n' '==> remove KumaBox/CNI network namespaces'
while IFS= read -r namespace; do
  [[ -n "$namespace" ]] || continue
  ip netns delete "$namespace" 2>/dev/null || true
done < <(ip netns list | awk '$1 ~ /^kb_/ {print $1}')

printf '%s\n' '==> remove KumaBox/CNI links and bridges'
while IFS= read -r link; do
  [[ -n "$link" ]] || continue
  ip link delete "$link" 2>/dev/null || true
done < <(ip -o link show | sed -nE 's/^[0-9]+: ([^:@]+).*/\1/p' | grep -E '^(kb|kbtap|kbcni|kumabox)' || true)

printf '%s\n' '==> remaining related processes'
ps -ef | grep -E '[c]ocoon|[c]loud-hypervisor|[b]enchmark-p6' || true
printf '%s\n' 'Environment reset completed.'
