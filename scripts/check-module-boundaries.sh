#!/usr/bin/env bash
set -euo pipefail

readonly repository_root="$(cd "$(dirname "$0")/.." && pwd)"
readonly module_prefix="github.com/Russell-Utopia/boss-job-agent"
readonly prototype_prefix="github.com/Russell-Utopia/boss-job-agent/throwaway-prototypes/"
packages="$(go list ./...)"

package_imports() {
	go list -f '{{join .Imports "\n"}}' "$1"
}

assert_no_exact_import() {
	local package_pattern="$1"
	local forbidden_import="$2"
	local imports
	imports="$(package_imports "$package_pattern")"
	if grep -Fxq "$forbidden_import" <<<"$imports"; then
		printf '%s must not import %s\n' "$package_pattern" "$forbidden_import" >&2
		exit 1
	fi
}

assert_no_import_prefix() {
	local package_pattern="$1"
	local forbidden_prefix="$2"
	local imports
	imports="$(package_imports "$package_pattern")"
	if grep -Fq "$forbidden_prefix" <<<"$imports"; then
		printf '%s must not import packages under %s\n' "$package_pattern" "$forbidden_prefix" >&2
		exit 1
	fi
}

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

for removed_package in application advice; do
	if [[ -d "$repository_root/internal/$removed_package" ]]; then
		printf 'removed package still exists: internal/%s\n' "$removed_package" >&2
		exit 1
	fi
done

business_modules=(onlineresume discovery jobpool assessment outreach automationsettings)

for forbidden_module in "${business_modules[@]}"; do
	assert_no_import_prefix \
		"./internal/sqlite/..." \
		"$module_prefix/internal/$forbidden_module"
done

if [[ -d "$repository_root/internal/runlog" ]]; then
	for forbidden_module in "${business_modules[@]}"; do
		assert_no_import_prefix \
			"./internal/runlog/..." \
			"$module_prefix/internal/$forbidden_module"
	done
fi

for forbidden_module in discovery assessment outreach automationsettings onlineresume; do
	assert_no_import_prefix \
		"./internal/jobpool/..." \
		"$module_prefix/internal/$forbidden_module"
done

for business_module in "${business_modules[@]}"; do
	package_pattern="./internal/$business_module/..."
	assert_no_import_prefix "$package_pattern" "$module_prefix/internal/app"
	assert_no_import_prefix "$package_pattern" "$module_prefix/internal/webui"
	assert_no_import_prefix "$package_pattern" "$module_prefix/internal/adapters"
	for other_module in "${business_modules[@]}"; do
		if [[ "$business_module" != "$other_module" ]]; then
			assert_no_import_prefix \
				"$package_pattern" \
				"$module_prefix/internal/$other_module/internal/sqlitedb"
		fi
	done
done

assert_no_import_prefix "./internal/webui" "$module_prefix/internal/app"
assert_no_import_prefix "./internal/webui" "$module_prefix/internal/sqlite"
assert_no_import_prefix "./internal/webui" "$module_prefix/internal/adapters"
assert_no_import_prefix "./internal/webui" "$module_prefix/internal/outreach"
assert_no_exact_import "./internal/webui" "database/sql"
assert_no_import_prefix "./..." "$module_prefix/docs"

if [[ ! -f "$repository_root/throwaway-prototypes/web-stack-comparison/go.mod" ]]; then
	printf 'throwaway Web comparison must remain an independent Go Module\n' >&2
	exit 1
fi

printf 'throwaway prototype packages are isolated from the application Go Module\n'
printf 'shared common, utils, domain, models, and repository packages are absent\n'
printf 'removed application and advice packages are absent\n'
printf 'stable business-module and Web import prohibitions hold\n'
