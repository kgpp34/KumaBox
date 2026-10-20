#!/bin/sh
# Build the Linux host release archive from one source revision.
set -eu

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
    echo "usage: $0 VERSION [OUTPUT_DIR]" >&2
    exit 2
fi

version=$1
output_dir=${2:-dist/release}
if ! printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$'; then
    echo "release version must look like v0.1.0 or v0.1.0-rc.1" >&2
    exit 2
fi
if [ -n "$(git status --porcelain --untracked-files=normal)" ]; then
    echo "release assets require a clean Git worktree" >&2
    exit 1
fi

commit=$(git rev-parse HEAD)
if git rev-parse -q --verify "refs/tags/$version^{commit}" >/dev/null; then
    tagged_commit=$(git rev-parse "refs/tags/$version^{commit}")
    if [ "$tagged_commit" != "$commit" ]; then
        echo "tag $version does not point to HEAD" >&2
        exit 1
    fi
fi

build_time=$(date -u +%Y-%m-%dT%H:%M:%SZ)
archive="kumabox_${version}_linux_amd64.tar.gz"
mkdir -p "$output_dir"
output_dir=$(cd "$output_dir" && pwd)
tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=readonly -trimpath \
    -ldflags "-s -w -X github.com/kumabox/kumabox/version.Version=$version -X github.com/kumabox/kumabox/version.Commit=$commit -X github.com/kumabox/kumabox/version.BuildTime=$build_time" \
    -o "$tmp_dir/kumabox" ./cmd/kumabox
install -m 0755 scripts/kumabox-check.sh "$tmp_dir/kumabox-check"
LC_ALL=C tar -C "$tmp_dir" -czf "$output_dir/$archive" kumabox kumabox-check

if command -v sha256sum >/dev/null 2>&1; then
    (cd "$output_dir" && sha256sum "$archive" > SHA256SUMS)
else
    (cd "$output_dir" && shasum -a 256 "$archive" > SHA256SUMS)
fi
printf '%s\n' "$output_dir/$archive" "$output_dir/SHA256SUMS"
