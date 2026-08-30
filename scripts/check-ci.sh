#!/usr/bin/env bash
set -euo pipefail

readonly repository_root="$(cd "$(dirname "$0")/.." && pwd)"
readonly workflow="$repository_root/.github/workflows/check.yml"

if [[ ! -f "$workflow" ]]; then
	printf 'missing GitHub Actions workflow: %s\n' "$workflow" >&2
	exit 1
fi

uses_lines="$(sed -n 's/^[[:space:]]*uses:[[:space:]]*//p' "$workflow")"
invalid_uses="$(printf '%s\n' "$uses_lines" | grep -Ev '^[^[:space:]@]+@[0-9a-f]{40}([[:space:]]+#.*)?$' || true)"
if [[ -z "$uses_lines" || -n "$invalid_uses" ]]; then
	printf 'every GitHub Action must use a full commit SHA; invalid entries:\n%s\n' "$invalid_uses" >&2
	exit 1
fi

run_lines="$(sed -n 's/^[[:space:]]*run:[[:space:]]*//p' "$workflow")"
if [[ "$run_lines" != "make check" ]]; then
	printf 'CI run steps must contain only the shared make check target; got:\n%s\n' "$run_lines" >&2
	exit 1
fi

if ! grep -Eq '^[[:space:]]+go-version-file:[[:space:]]+go\.mod$' "$workflow"; then
	printf 'CI must read the Go version from go.mod\n' >&2
	exit 1
fi
if ! grep -Eq '^[[:space:]]+check-latest:[[:space:]]+false$' "$workflow"; then
	printf 'CI must not replace the pinned Go version with the latest release\n' >&2
	exit 1
fi

printf 'GitHub Actions: one shared make check with SHA-pinned actions\n'
