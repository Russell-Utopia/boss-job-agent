#!/usr/bin/env bash
set -euo pipefail

readonly repository_root="$(cd "$(dirname "$0")/.." && pwd)"
readonly prototype_prefix="github.com/Russell-Utopia/boss-job-agent/throwaway-prototypes/"
packages="$(go list ./...)"

if grep -Fq "$prototype_prefix" <<<"$packages"; then
	printf 'throwaway prototype packages are part of the application Go Module\n' >&2
	exit 1
fi

for forbidden_package in common utils domain models repository; do
	if [[ -d "$repository_root/internal/$forbidden_package" ]]; then
		printf 'forbidden shared package exists: internal/%s\n' "$forbidden_package" >&2
		exit 1
	fi
done

if [[ ! -f "$repository_root/throwaway-prototypes/web-stack-comparison/go.mod" ]]; then
	printf 'throwaway Web comparison must remain an independent Go Module\n' >&2
	exit 1
fi

printf 'throwaway prototype packages are isolated from the application Go Module\n'
printf 'shared common, utils, domain, models, and repository packages are absent\n'
