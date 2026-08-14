#!/usr/bin/env bash
set -Eeuo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
checker="$repo_root/doctor/check.sh"

help=$($checker --help)
grep -Fq -- '--subnet CIDR' <<< "$help"
grep -Fq -- '--metadata-backend NAME' <<< "$help"

if "$checker" --subnet >/dev/null 2>&1; then
	echo "kumabox-check accepted --subnet without a value" >&2
	exit 1
fi
if "$checker" --metadata-backend invalid >/dev/null 2>&1; then
	echo "kumabox-check accepted an invalid metadata backend" >&2
	exit 1
fi

echo "release host-check tests passed"
