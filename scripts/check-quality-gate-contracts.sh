#!/usr/bin/env bash
set -euo pipefail

readonly repository_root="$(cd "$(dirname "$0")/.." && pwd)"

if output="$(COVERAGE_MINIMUM=0 "$repository_root/scripts/check-coverage.sh" 2>&1)"; then
	printf 'COVERAGE_MINIMUM=0 unexpectedly disabled the repository coverage floor\n' >&2
	exit 1
fi
if ! grep -Fq 'cannot be lower than repository minimum' <<<"$output"; then
	printf 'COVERAGE_MINIMUM=0 failed for the wrong reason:\n%s\n' "$output" >&2
	exit 1
fi

printf 'coverage minimum cannot be lowered through COVERAGE_MINIMUM\n'
