#!/bin/sh
# Build one version-matched boot -> base -> single-layer runtime chain.
set -eu
platform=linux/amd64
boot_image=kumabox/boot:6.18.37
base_image=kumabox/base:24.04
runtime_image=kumabox/rt:24.04
skip_boot=false
while [ "$#" -gt 0 ]; do
    case "$1" in
        --platform|--boot-image|--base-image|--tag)
            [ "$#" -ge 2 ] || { echo "missing value for $1" >&2; exit 2; }
            case "$1" in
                --platform) platform=$2 ;;
                --boot-image) boot_image=$2 ;;
                --base-image) base_image=$2 ;;
                --tag) runtime_image=$2 ;;
            esac
            shift 2 ;;
        --skip-boot) skip_boot=true; shift ;;
        *) echo "unknown argument: $1" >&2; exit 2 ;;
    esac
done
[ -f oci-images/base/24.04/Dockerfile ] || { echo 'run from the repository root' >&2; exit 2; }
case "$platform" in linux/amd64|linux/arm64) ;; *) echo 'unsupported guest platform' >&2; exit 2 ;; esac
privileged=false
if ! docker info >/dev/null 2>&1; then sudo -n docker info >/dev/null; privileged=true; fi
docker_command() {
    if "$privileged"; then sudo -n docker "$@"; else docker "$@"; fi
}
version=${KUMABOX_VERSION:-dev}
revision=${KUMABOX_COMMIT:-$(git rev-parse HEAD)}
built=${KUMABOX_BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
if ! "$skip_boot"; then
    docker_command build --platform "$platform" -f oci-images/boot/Dockerfile -t "$boot_image" .
fi
docker_command build --platform "$platform" -f oci-images/base/24.04/Dockerfile \
    --build-arg "BOOT_IMAGE=$boot_image" --build-arg "UBUNTU_IMAGE=${UBUNTU_IMAGE:-ubuntu:24.04}" \
    --build-arg "GOPROXY=${GOPROXY:-https://proxy.golang.org,direct}" \
    --build-arg "KUMABOX_VERSION=$version" --build-arg "KUMABOX_COMMIT=$revision" \
    --build-arg "KUMABOX_BUILD_TIME=$built" -t "$base_image" .
docker_command build --platform "$platform" -f oci-images/rt/24.04/Dockerfile \
    --build-arg "BASE_IMAGE=$base_image" --build-arg "KUMABOX_VERSION=$version" \
    --build-arg "KUMABOX_COMMIT=$revision" --build-arg "KUMABOX_BUILD_TIME=$built" \
    -t "$runtime_image" .
printf 'Built %s (%s) from %s and %s\n' "$runtime_image" "$revision" "$base_image" "$boot_image"
