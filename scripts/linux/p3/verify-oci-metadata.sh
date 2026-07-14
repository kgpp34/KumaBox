#!/usr/bin/env bash
set -euo pipefail

kumabox_path="./bin/kumabox"
root_dir="/tmp/kumabox-p0/data"
ref="kumabox/ubuntu:24.04-p3"
platform="linux/amd64"
name="p3-metadata"
source="auto"
mkfs_erofs="mkfs.erofs"
use_sudo=false

usage() {
  cat <<'USAGE'
Usage: scripts/linux/p3/verify-oci-metadata.sh [options]

Options:
  --kumabox PATH      kumabox binary path, defaults to ./bin/kumabox
  --root-dir PATH     state root directory, defaults to /tmp/kumabox-p0/data
  --ref REF           OCI image ref, defaults to kumabox/ubuntu:24.04-p3
  --platform VALUE    OCI platform, defaults to linux/amd64
  --name NAME         image name, defaults to p3-metadata
  --source VALUE      OCI source: auto, registry, or daemon. Defaults to auto
  --mkfs-erofs PATH   mkfs.erofs binary path, defaults to mkfs.erofs
  --sudo              run kumabox through sudo for root-owned state dirs

Verifies OCI metadata mapping:
image build records OCI config env/cmd/entrypoint/workdir/user/labels semantics,
marks agent injection as deferred, and image inspect exposes the same metadata.
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

step() {
  printf '\n==> %s\n' "$1"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --kumabox) require_value "$1" "${2:-}"; kumabox_path="$2"; shift 2 ;;
    --root-dir) require_value "$1" "${2:-}"; root_dir="$2"; shift 2 ;;
    --ref) require_value "$1" "${2:-}"; ref="$2"; shift 2 ;;
    --platform) require_value "$1" "${2:-}"; platform="$2"; shift 2 ;;
    --name) require_value "$1" "${2:-}"; name="$2"; shift 2 ;;
    --source) require_value "$1" "${2:-}"; source="$2"; shift 2 ;;
    --mkfs-erofs) require_value "$1" "${2:-}"; mkfs_erofs="$2"; shift 2 ;;
    --sudo) use_sudo=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [[ ! -x "$kumabox_path" ]]; then
  echo "kumabox is not executable: $kumabox_path" >&2
  exit 1
fi
if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required for OCI metadata verification" >&2
  exit 1
fi
if [[ "$mkfs_erofs" == */* ]]; then
  if [[ ! -x "$mkfs_erofs" ]]; then
    echo "mkfs.erofs is not executable: $mkfs_erofs" >&2
    exit 1
  fi
elif ! command -v "$mkfs_erofs" >/dev/null 2>&1; then
  echo "mkfs.erofs is required for OCI metadata verification" >&2
  exit 1
fi
if [[ "$use_sudo" == true ]]; then
  if ! command -v sudo >/dev/null 2>&1; then
    echo "--sudo requested but sudo is missing" >&2
    exit 1
  fi
  kumabox_cmd=(sudo "$kumabox_path")
else
  kumabox_cmd=("$kumabox_path")
fi

kb() {
  "${kumabox_cmd[@]}" --root-dir "$root_dir" "$@"
}

step "clean previous OCI metadata image record"
kb image rm "$name" >/dev/null 2>&1 || true

step "build OCI image and capture metadata"
build_json="$(kb image build "$ref" \
  --name "$name" \
  --platform "$platform" \
  --source "$source" \
  --mkfs-erofs "$mkfs_erofs" \
  --json)"
printf '%s\n' "$build_json"

agent_injection="$(printf '%s' "$build_json" | jq -r '.oci.agentInjection')"
cmd_len="$(printf '%s' "$build_json" | jq '.oci.imageConfig.cmd | length')"
env_has_debian_frontend="$(printf '%s' "$build_json" | jq 'any(.oci.imageConfig.env[]?; . == "DEBIAN_FRONTEND=noninteractive")')"
cmd_has_init="$(printf '%s' "$build_json" | jq 'any(.oci.imageConfig.cmd[]?; . == "/sbin/init")')"

if [[ "$agent_injection" != "deferred" ]]; then
  echo "agent injection marker mismatch: $agent_injection" >&2
  exit 1
fi
if [[ "$cmd_len" -lt 1 || "$cmd_has_init" != "true" ]]; then
  echo "OCI cmd metadata is missing /sbin/init" >&2
  exit 1
fi
if [[ "$env_has_debian_frontend" != "true" ]]; then
  echo "OCI env metadata is missing DEBIAN_FRONTEND=noninteractive" >&2
  exit 1
fi
printf 'state: agentInjection=%s cmdLen=%s envHasDebianFrontend=%s\n' "$agent_injection" "$cmd_len" "$env_has_debian_frontend"

step "inspect image metadata"
inspect_json="$(kb image inspect "$name" --json)"
printf '%s\n' "$inspect_json" | jq '.oci.imageConfig, .oci.agentInjection'

inspect_agent="$(printf '%s' "$inspect_json" | jq -r '.oci.agentInjection')"
inspect_cmd_has_init="$(printf '%s' "$inspect_json" | jq 'any(.oci.imageConfig.cmd[]?; . == "/sbin/init")')"
if [[ "$inspect_agent" != "deferred" || "$inspect_cmd_has_init" != "true" ]]; then
  echo "inspect metadata does not match build output" >&2
  exit 1
fi

step "cleanup image record"
kb image rm "$name" >/dev/null

echo "P3-07 OCI metadata mapping verification passed"
