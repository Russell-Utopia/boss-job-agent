#!/usr/bin/env bash
set -euo pipefail

readonly repository_root="$(cd "$(dirname "$0")/.." && pwd)"
readonly minimum_coverage="${COVERAGE_MINIMUM:-60.0}"
temporary_directory="$(mktemp -d)"
trap 'rm -rf "$temporary_directory"' EXIT

raw_profile="$temporary_directory/coverage.raw.out"
filtered_profile="$temporary_directory/coverage.out"
generated_files="$temporary_directory/generated-files.txt"

cd "$repository_root"
go test -count=1 -coverpkg=./... -coverprofile="$raw_profile" ./...

module_path="$(go list -m -f '{{.Path}}')"
printf '__no_generated_files__\n' >"$generated_files"
while IFS= read -r -d '' file; do
	if grep -Eq '^// Code generated .* DO NOT EDIT\.$' "$file"; then
		printf '%s/%s\n' "$module_path" "${file#./}" >>"$generated_files"
	fi
done < <(
	find . \
		\( -path './.git' -o -path './.cache' -o -path './throwaway-prototypes' -o -path './tools' \) -prune \
		-o -type f -name '*.go' -print0
)

awk '
	NR == FNR { generated[$0] = 1; next }
	FNR == 1 { print; next }
	{
		source = $1
		sub(/:[0-9]+\..*$/, "", source)
		if (!(source in generated)) print
	}
' "$generated_files" "$raw_profile" >"$filtered_profile"

coverage_summary="$(go tool cover -func="$filtered_profile")"
total_line="$(printf '%s\n' "$coverage_summary" | awk '$1 == "total:" { print }')"
actual_coverage="$(printf '%s\n' "$total_line" | awk '{ gsub(/%/, "", $3); print $3 }')"
if [[ -z "$actual_coverage" ]]; then
	printf 'coverage total is missing\n' >&2
	exit 1
fi

printf '%s\n' "$total_line"
if ! awk -v actual="$actual_coverage" -v minimum="$minimum_coverage" \
	'BEGIN { exit !(actual + 0 >= minimum + 0) }'; then
	printf 'coverage %.1f%% is below %.1f%%\n' "$actual_coverage" "$minimum_coverage" >&2
	exit 1
fi

printf 'coverage %.1f%% meets %.1f%%\n' "$actual_coverage" "$minimum_coverage"
