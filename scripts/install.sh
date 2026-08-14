#!/usr/bin/env sh
set -eu

repository=${KUMABOX_REPOSITORY:-kgpp34/KumaBox}
version=${KUMABOX_VERSION:-latest}
install_dir=${KUMABOX_INSTALL_DIR:-/usr/local/bin}
github_api_url=${KUMABOX_GITHUB_API_URL:-https://api.github.com}
release_base_url=${KUMABOX_RELEASE_BASE_URL:-}

usage() {
	cat <<'EOF'
Usage: install.sh [--version VERSION] [--install-dir DIR]

Download a KumaBox GitHub Release, verify its SHA256 checksum, and install
kumabox and kumabox-check. The script supports Linux amd64 and arm64 hosts.

Environment overrides:
  KUMABOX_REPOSITORY   GitHub owner/repository (default: kgpp34/KumaBox)
  KUMABOX_VERSION      release tag or latest
  KUMABOX_INSTALL_DIR  destination directory (default: /usr/local/bin)
  KUMABOX_GITHUB_API_URL    GitHub-compatible API endpoint
  KUMABOX_RELEASE_BASE_URL  release directory URL for mirrors and air gaps
EOF
}

while [ "$#" -gt 0 ]; do
	case "$1" in
		--version) [ "$#" -ge 2 ] || { echo "--version requires a value" >&2; exit 2; }; version=$2; shift 2 ;;
		--install-dir) [ "$#" -ge 2 ] || { echo "--install-dir requires a value" >&2; exit 2; }; install_dir=$2; shift 2 ;;
		-h|--help) usage; exit 0 ;;
		*) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
	esac
done

[ "$(uname -s)" = Linux ] || { echo "KumaBox requires Linux" >&2; exit 1; }
case "$(uname -m)" in
	x86_64) arch=amd64 ;;
	aarch64|arm64) arch=arm64 ;;
	*) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

for command_name in curl install tar; do
	command -v "$command_name" >/dev/null 2>&1 || { echo "$command_name is required" >&2; exit 1; }
done
if command -v sha256sum >/dev/null 2>&1; then
	sha256_file() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
	sha256_file() { shasum -a 256 "$1" | awk '{print $1}'; }
else
	echo "sha256sum or shasum is required" >&2
	exit 1
fi

if [ "$version" = latest ]; then
	version=$(curl -fsSL --retry 3 "${github_api_url%/}/repos/${repository}/releases/latest" |
		sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
	[ -n "$version" ] || { echo "cannot resolve latest KumaBox release" >&2; exit 1; }
fi

archive="kumabox-${version}-linux-${arch}.tar.gz"
if [ -n "$release_base_url" ]; then
	base_url=${release_base_url%/}
else
	base_url="https://github.com/${repository}/releases/download/${version}"
fi
temporary_dir=$(mktemp -d)
trap 'rm -rf "$temporary_dir"' EXIT HUP INT TERM

echo "Downloading KumaBox ${version} for linux/${arch}"
curl -fsSL --retry 3 -o "$temporary_dir/$archive" "$base_url/$archive"
curl -fsSL --retry 3 -o "$temporary_dir/$archive.sha256" "$base_url/$archive.sha256"

expected=$(awk '{print $1; exit}' "$temporary_dir/$archive.sha256")
actual=$(sha256_file "$temporary_dir/$archive")
[ -n "$expected" ] && [ "$actual" = "$expected" ] || {
	echo "checksum verification failed for $archive" >&2
	exit 1
}

mkdir -p "$temporary_dir/extract"
tar -xzf "$temporary_dir/$archive" -C "$temporary_dir/extract"
for file in kumabox kumabox-check; do
	[ -f "$temporary_dir/extract/$file" ] || { echo "release archive is missing $file" >&2; exit 1; }
done

install -d -m 0755 "$install_dir"
install -m 0755 "$temporary_dir/extract/kumabox" "$install_dir/kumabox"
install -m 0755 "$temporary_dir/extract/kumabox-check" "$install_dir/kumabox-check"

echo "Installed KumaBox ${version} to $install_dir"
echo "Next: sudo $install_dir/kumabox-check --upgrade"
