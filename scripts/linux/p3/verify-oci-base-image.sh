#!/usr/bin/env bash
set -euo pipefail

context_dir="oci-images/ubuntu"
dockerfile="oci-images/ubuntu/24.04/Dockerfile"
tag="kumabox/ubuntu:24.04-p3"
platform="linux/amd64"
fixture_dir="/tmp/kumabox-p0/fixtures/oci"
skip_build=0
keep_fixture=0
apt_mirror="${KUMABOX_APT_MIRROR:-}"
apt_security_mirror="${KUMABOX_APT_SECURITY_MIRROR:-}"
docker_network=""
agent_binary="kumabox-agent-linux-amd64"

usage() {
  cat <<'USAGE'
Usage: scripts/linux/p3/verify-oci-base-image.sh [options]

Options:
  --context-dir PATH   Docker build context, defaults to oci-images/ubuntu
  --dockerfile PATH    Dockerfile path, defaults to oci-images/ubuntu/24.04/Dockerfile
  --tag REF            Local image tag, defaults to kumabox/ubuntu:24.04-p3
  --platform PLATFORM  Docker build platform, defaults to linux/amd64
  --fixture-dir PATH   Output fixture directory, defaults to /tmp/kumabox-p0/fixtures/oci
  --skip-build         Verify an existing local Docker image tag
  --keep-fixture       Keep existing fixture directory contents
  --apt-mirror URL     Override Ubuntu archive mirror, or set KUMABOX_APT_MIRROR
  --apt-security-mirror URL
                       Override Ubuntu security mirror, or set KUMABOX_APT_SECURITY_MIRROR
  --docker-network NET Pass --network NET to docker build, for example host

Builds and verifies the P3-00 KumaBox-compatible OCI VM base image.
It does not boot a microVM. It checks that the image contains kernel/initrd,
KumaBox initramfs hooks, systemd baseline, and the reserved agent service path.
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

pass() {
  printf 'pass: %s\n' "$1"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --context-dir)
      require_value "$1" "${2:-}"
      context_dir="$2"
      shift 2
      ;;
    --dockerfile)
      require_value "$1" "${2:-}"
      dockerfile="$2"
      shift 2
      ;;
    --tag)
      require_value "$1" "${2:-}"
      tag="$2"
      shift 2
      ;;
    --platform)
      require_value "$1" "${2:-}"
      platform="$2"
      shift 2
      ;;
    --fixture-dir)
      require_value "$1" "${2:-}"
      fixture_dir="$2"
      shift 2
      ;;
    --skip-build)
      skip_build=1
      shift
      ;;
    --keep-fixture)
      keep_fixture=1
      shift
      ;;
    --apt-mirror)
      require_value "$1" "${2:-}"
      apt_mirror="$2"
      shift 2
      ;;
    --apt-security-mirror)
      require_value "$1" "${2:-}"
      apt_security_mirror="$2"
      shift 2
      ;;
    --docker-network)
      require_value "$1" "${2:-}"
      docker_network="$2"
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

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "verify-oci-base-image must run on Linux" >&2
  exit 1
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "docker is required for P3-00 base image verification" >&2
  exit 1
fi

if [[ ! -d "$context_dir" ]]; then
  echo "context directory does not exist: $context_dir" >&2
  exit 1
fi
if [[ ! -f "$dockerfile" ]]; then
  echo "Dockerfile does not exist: $dockerfile" >&2
  exit 1
fi

step "prepare fixture directory"
if [[ "$keep_fixture" -eq 0 ]]; then
  rm -rf "$fixture_dir"
fi
mkdir -p "$fixture_dir"
pass "fixture directory ready: $fixture_dir"

if [[ "$skip_build" -eq 0 ]]; then
  step "build KumaBox guest agent"
  if ! command -v go >/dev/null 2>&1; then
    echo "go is required to build kumabox-agent" >&2
    exit 1
  fi
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$context_dir/$agent_binary" ./cmd/kumabox-agent
  pass "guest agent binary ready: $context_dir/$agent_binary"

  step "build KumaBox OCI VM base image"
  build_args=()
  if [[ -n "$apt_mirror" ]]; then
    build_args+=(--build-arg "APT_MIRROR=$apt_mirror")
    if [[ -z "$apt_security_mirror" ]]; then
      apt_security_mirror="$apt_mirror"
    fi
  fi
  if [[ -n "$apt_security_mirror" ]]; then
    build_args+=(--build-arg "APT_SECURITY_MIRROR=$apt_security_mirror")
  fi
  if [[ -n "$docker_network" ]]; then
    build_args+=(--network "$docker_network")
  fi
  docker build \
    "${build_args[@]}" \
    --platform "$platform" \
    -f "$dockerfile" \
    -t "$tag" \
    "$context_dir"
else
  step "verify existing local image"
fi

image_id="$(docker image inspect "$tag" --format '{{.Id}}')"
printf 'state: image=%s id=%s platform=%s\n' "$tag" "$image_id" "$platform"
printf '%s\n' "$image_id" >"$fixture_dir/image-id.txt"
printf '%s\n' "$tag" >"$fixture_dir/image-ref.txt"
pass "image is available locally"

run_check() {
  local name="$1"
  local script="$2"
  if docker run --rm --entrypoint /bin/sh "$tag" -eu -c "$script"; then
    pass "$name"
    return
  fi
  echo "failed: $name" >&2
  exit 1
}

step "inspect boot assets"
run_check "kernel: /boot/vmlinuz-* exists" 'ls /boot/vmlinuz-* >/dev/null'
run_check "initrd: /boot/initrd.img-* exists" 'ls /boot/initrd.img-* >/dev/null'

step "inspect initramfs hooks"
run_check "overlay hook installed" 'test -x /etc/initramfs-tools/scripts/kumabox-overlay'
run_check "network hook installed" 'test -x /etc/initramfs-tools/scripts/init-bottom/kumabox-network'
run_check "overlay hook reads kumabox.layers" 'grep -q "kumabox.layers=" /etc/initramfs-tools/scripts/kumabox-overlay'
run_check "overlay hook reads kumabox.cow" 'grep -q "kumabox.cow=" /etc/initramfs-tools/scripts/kumabox-overlay'
run_check "overlay hook preserves COW freeze mount" 'grep -q "expose COW freeze mount" /etc/initramfs-tools/scripts/kumabox-overlay'
run_check "network hook reads kumabox.hostname" 'grep -q "kumabox.hostname=" /etc/initramfs-tools/scripts/init-bottom/kumabox-network'
run_check "initramfs includes overlay hook" 'initrd="$(ls /boot/initrd.img-* | head -n 1)"; lsinitramfs "$initrd" | grep -q "scripts/kumabox-overlay"'
run_check "initramfs includes network hook" 'initrd="$(ls /boot/initrd.img-* | head -n 1)"; lsinitramfs "$initrd" | grep -q "scripts/init-bottom/kumabox-network"'

step "inspect kernel module profile"
run_check "erofs module requested" 'grep -qx "erofs" /etc/initramfs-tools/modules'
run_check "overlay module requested" 'grep -qx "overlay" /etc/initramfs-tools/modules'
run_check "ext4 module requested" 'grep -qx "ext4" /etc/initramfs-tools/modules'
run_check "virtio net module requested" 'grep -qx "virtio_net" /etc/initramfs-tools/modules'
run_check "vsock module requested" 'grep -qx "vsock" /etc/initramfs-tools/modules'

step "inspect guest baseline"
run_check "systemd init exists" 'test -e /sbin/init'
run_check "networkd default config exists" 'test -f /etc/systemd/network/20-wired.network'
run_check "networkd uses MAC DHCP identity" 'grep -q "ClientIdentifier=mac" /etc/systemd/network/20-wired.network'
run_check "agent path reserved" 'test -x /usr/local/bin/kumabox-agent'
run_check "agent binary reports version" '/usr/local/bin/kumabox-agent version | grep -Eq "^[0-9]+\\.[0-9]+\\.[0-9]+$"'
run_check "agent unit installed" 'test -f /etc/systemd/system/kumabox-agent.service'
run_check "agent unit points at kumabox-agent serve" 'grep -q "ExecStart=/usr/local/bin/kumabox-agent serve" /etc/systemd/system/kumabox-agent.service'
run_check "agent unit logs to console" 'grep -q "StandardError=journal+console" /etc/systemd/system/kumabox-agent.service'
run_check "agent unit enabled" 'test -e /etc/systemd/system/multi-user.target.wants/kumabox-agent.service'

step "write fixture manifest"
cat >"$fixture_dir/manifest.json" <<EOF
{
  "schemaVersion": "kumabox.oci-base-image.v1",
  "image": "$tag",
  "imageId": "$image_id",
  "platform": "$platform",
  "requirements": {
    "kernel": true,
    "initrd": true,
    "erofs": true,
    "overlayfs": true,
    "ext4": true,
    "vsock": true,
    "agentReserved": true
  }
}
EOF
cat "$fixture_dir/manifest.json"

echo "P3-00 OCI VM base image verification passed"
