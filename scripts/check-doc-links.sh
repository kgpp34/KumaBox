#!/usr/bin/env bash

set -euo pipefail

failed=0
while IFS= read -r document; do
  while IFS= read -r markdown_link; do
    target="${markdown_link#](}"
    target="${target%)}"
    target="${target%%#*}"
    case "${target}" in
      ""|http://*|https://*|mailto:*) continue ;;
    esac
    if [[ ! -e "$(dirname "${document}")/${target}" ]]; then
      printf 'broken Markdown link: %s -> %s\n' "${document}" "${target}" >&2
      failed=1
    fi
  done < <(grep -Eo '\]\([^)]+(\.md|LICENSE)(#[^)]*)?\)' "${document}" || true)
done < <(git ls-files '*.md')

exit "${failed}"
