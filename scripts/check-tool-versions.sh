#!/usr/bin/env bash
set -euo pipefail

readonly repository_root="$(cd "$(dirname "$0")/.." && pwd)"

# shellcheck disable=SC1091 # The path is repository-relative and fixed above.
source "$repository_root/tools/versions.env"

actual_go_version="$(go env GOVERSION)"
expected_go_version="go${GO_VERSION}"
if [[ "$actual_go_version" != "$expected_go_version" ]]; then
	printf 'Go toolchain is %s, want %s\n' "$actual_go_version" "$expected_go_version" >&2
	exit 1
fi

printf 'Go toolchain: %s\n' "$actual_go_version"

check_tool_module() {
	local module_path="$1"
	local expected_version="$2"
	local actual_version

	actual_version="$(go -C "$repository_root/tools" list -m -f '{{.Version}}' "$module_path")"
	if [[ "$actual_version" != "v${expected_version}" ]]; then
		printf '%s is %s, want v%s\n' "$module_path" "$actual_version" "$expected_version" >&2
		exit 1
	fi
	printf '%s: %s\n' "$module_path" "$actual_version"
}

check_tool_module "golang.org/x/vuln" "$GOVULNCHECK_VERSION"

if ! go -C "$repository_root/tools" list tool | grep -Fxq "golang.org/x/vuln/cmd/govulncheck"; then
	printf 'govulncheck is not declared as a Go tool\n' >&2
	exit 1
fi

printf 'github.com/sqlc-dev/sqlc: v%s (release binary)\n' "$SQLC_VERSION"
printf 'github.com/pressly/goose/v3: v%s (reserved for the migration slice)\n' "$GOOSE_VERSION"
