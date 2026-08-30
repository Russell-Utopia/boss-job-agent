#!/usr/bin/env bash
set -euo pipefail

readonly repository_root="$(cd "$(dirname "$0")/.." && pwd)"

# shellcheck disable=SC1091 # The path is repository-relative and fixed above.
source "$repository_root/tools/versions.env"

read_directive() {
	local file="$1"
	local directive="$2"

	awk -v directive="$directive" '$1 == directive { print $2 }' "$file"
}

readonly root_go_version="$(read_directive "$repository_root/go.mod" go)"
readonly root_toolchain="$(read_directive "$repository_root/go.mod" toolchain)"
readonly tools_go_version="$(read_directive "$repository_root/tools/go.mod" go)"
readonly tools_toolchain="$(read_directive "$repository_root/tools/go.mod" toolchain)"
readonly toolchain_version="${root_toolchain#go}"
readonly toolchain_language_version="${toolchain_version%.*}"

if [[ ! "$root_go_version" =~ ^[0-9]+\.[0-9]+$ ]]; then
	printf 'root go.mod must declare one Go language version; got %q\n' "$root_go_version" >&2
	exit 1
fi
if [[ ! "$root_toolchain" =~ ^go[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	printf 'root go.mod must declare one exact Go toolchain; got %q\n' "$root_toolchain" >&2
	exit 1
fi
if [[ "$toolchain_language_version" != "$root_go_version" ]]; then
	printf 'root go.mod language version is %s but toolchain is %s\n' "$root_go_version" "$root_toolchain" >&2
	exit 1
fi
if [[ "$tools_go_version" != "$root_go_version" ]]; then
	printf 'tools/go.mod Go version is %s, want %s\n' "${tools_go_version:-missing}" "$root_go_version" >&2
	exit 1
fi
if [[ "$tools_toolchain" != "$root_toolchain" ]]; then
	printf 'tools/go.mod toolchain is %s, want %s\n' "${tools_toolchain:-missing}" "$root_toolchain" >&2
	exit 1
fi

actual_go_version="$(go env GOVERSION)"
if [[ "$actual_go_version" != "$root_toolchain" ]]; then
	printf 'Go toolchain is %s, want %s\n' "$actual_go_version" "$root_toolchain" >&2
	exit 1
fi

printf 'Go version contract: language %s, toolchain %s (root and tools)\n' "$root_go_version" "$root_toolchain"
printf 'Go toolchain in use: %s\n' "$actual_go_version"

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

actual_goose_version="$(go -C "$repository_root" list -m -f '{{.Version}}' github.com/pressly/goose/v3)"
if [[ "$actual_goose_version" != "v${GOOSE_VERSION}" ]]; then
	printf 'github.com/pressly/goose/v3 is %s, want v%s\n' "$actual_goose_version" "$GOOSE_VERSION" >&2
	exit 1
fi
printf 'github.com/pressly/goose/v3: %s (runtime migration provider)\n' "$actual_goose_version"
