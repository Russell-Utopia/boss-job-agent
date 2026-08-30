#!/usr/bin/env bash
set -euo pipefail

readonly repository_root="$(cd "$(dirname "$0")/.." && pwd)"

unformatted="$({
	find "$repository_root" \
		\( -path "$repository_root/.git" \
		-o -path "$repository_root/.cache" \
		-o -path "$repository_root/throwaway-prototypes" \
		-o -path "$repository_root/tools" \) -prune \
		-o -type f -name '*.go' -print0
} | xargs -0 gofmt -l)"

if [[ -n "$unformatted" ]]; then
	printf 'Go files are not formatted:\n%s\n' "$unformatted" >&2
	exit 1
fi

printf 'Go format: clean\n'
