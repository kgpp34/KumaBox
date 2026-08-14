#!/usr/bin/env bash
set -Eeuo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT

mkdir -p "$work_dir/bin" "$work_dir/release" "$work_dir/payload"
cat > "$work_dir/bin/uname" <<'EOF'
#!/usr/bin/env sh
case "${1:-}" in
  -s) printf 'Linux\n' ;;
  -m) printf 'x86_64\n' ;;
  *) printf 'Linux\n' ;;
esac
EOF
chmod +x "$work_dir/bin/uname"

printf '#!/usr/bin/env sh\nprintf "kumabox fixture\\n"\n' > "$work_dir/payload/kumabox"
printf '#!/usr/bin/env sh\nprintf "check fixture\\n"\n' > "$work_dir/payload/kumabox-check"
chmod +x "$work_dir/payload/kumabox" "$work_dir/payload/kumabox-check"

archive=kumabox-vtest-linux-amd64.tar.gz
tar -C "$work_dir/payload" -czf "$work_dir/release/$archive" kumabox kumabox-check
if command -v sha256sum >/dev/null 2>&1; then
	sha256sum "$work_dir/release/$archive" > "$work_dir/release/$archive.sha256"
else
	shasum -a 256 "$work_dir/release/$archive" > "$work_dir/release/$archive.sha256"
fi

PATH="$work_dir/bin:$PATH" \
	KUMABOX_RELEASE_BASE_URL="file://$work_dir/release" \
	sh "$repo_root/scripts/install.sh" --version vtest --install-dir "$work_dir/install"

"$work_dir/install/kumabox" | grep -Fx 'kumabox fixture'
"$work_dir/install/kumabox-check" | grep -Fx 'check fixture'

tar -C "$work_dir/payload" -czf "$work_dir/release/$archive" kumabox
if command -v sha256sum >/dev/null 2>&1; then
	sha256sum "$work_dir/release/$archive" > "$work_dir/release/$archive.sha256"
else
	shasum -a 256 "$work_dir/release/$archive" > "$work_dir/release/$archive.sha256"
fi
if PATH="$work_dir/bin:$PATH" \
	KUMABOX_RELEASE_BASE_URL="file://$work_dir/release" \
	sh "$repo_root/scripts/install.sh" --version vtest --install-dir "$work_dir/incomplete" \
	>/dev/null 2>&1; then
	echo "installer accepted an incomplete release archive" >&2
	exit 1
fi

printf '0  %s\n' "$archive" > "$work_dir/release/$archive.sha256"
if PATH="$work_dir/bin:$PATH" \
	KUMABOX_RELEASE_BASE_URL="file://$work_dir/release" \
	sh "$repo_root/scripts/install.sh" --version vtest --install-dir "$work_dir/rejected" \
	>/dev/null 2>&1; then
	echo "installer accepted an invalid checksum" >&2
	exit 1
fi

echo "release installer tests passed"
