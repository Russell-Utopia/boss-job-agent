#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C

readonly repository_root="$(cd "$(dirname "$0")/.." && pwd)"

# shellcheck disable=SC1091 # The path is repository-relative and fixed above.
source "$repository_root/tools/versions.env"

tool="${1:-}"
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$(uname -m)" in
	x86_64) arch="amd64" ;;
	arm64 | aarch64) arch="arm64" ;;
	*)
		printf 'unsupported architecture: %s\n' "$(uname -m)" >&2
		exit 1
		;;
esac

platform_key="$(printf '%s_%s' "$os" "$arch" | tr '[:lower:]' '[:upper:]')"
case "$tool" in
	golangci-lint)
		version="$GOLANGCI_LINT_VERSION"
		archive="golangci-lint-${version}-${os}-${arch}.tar.gz"
		url="https://github.com/golangci/golangci-lint/releases/download/v${version}/${archive}"
		checksum_variable="GOLANGCI_LINT_${platform_key}_SHA256"
		extracted_binary="golangci-lint-${version}-${os}-${arch}/golangci-lint"
		version_arguments=(version)
		;;
	sqlc)
		version="$SQLC_VERSION"
		archive="sqlc_${version}_${os}_${arch}.tar.gz"
		url="https://github.com/sqlc-dev/sqlc/releases/download/v${version}/${archive}"
		checksum_variable="SQLC_${platform_key}_SHA256"
		extracted_binary="sqlc"
		version_arguments=(version)
		;;
	*)
		printf 'usage: %s {golangci-lint|sqlc}\n' "$0" >&2
		exit 2
		;;
esac

expected_checksum="${!checksum_variable:-}"
if [[ -z "$expected_checksum" ]]; then
	printf '%s v%s has no pinned checksum for %s/%s\n' "$tool" "$version" "$os" "$arch" >&2
	exit 1
fi

destination_directory="$repository_root/.cache/tools/$tool/$version"
destination="$destination_directory/$tool"
if [[ -x "$destination" ]]; then
	installed_version="$("$destination" "${version_arguments[@]}" 2>&1 || true)"
	if grep -Fq "$version" <<<"$installed_version"; then
		printf '%s\n' "$destination"
		exit 0
	fi
fi

temporary_directory="$(mktemp -d)"
trap 'rm -rf "$temporary_directory"' EXIT

curl --proto '=https' --tlsv1.2 --location --fail --silent --show-error \
	--output "$temporary_directory/$archive" "$url"

if command -v sha256sum >/dev/null 2>&1; then
	actual_checksum="$(sha256sum "$temporary_directory/$archive" | awk '{print $1}')"
else
	actual_checksum="$(shasum -a 256 "$temporary_directory/$archive" | awk '{print $1}')"
fi
if [[ "$actual_checksum" != "$expected_checksum" ]]; then
	printf '%s checksum is %s, want %s\n' "$archive" "$actual_checksum" "$expected_checksum" >&2
	exit 1
fi

tar -xzf "$temporary_directory/$archive" -C "$temporary_directory"
mkdir -p "$destination_directory"
install -m 0755 "$temporary_directory/$extracted_binary" "$destination"
installed_version="$("$destination" "${version_arguments[@]}")"
if ! grep -Fq "$version" <<<"$installed_version"; then
	printf '%s reports an unexpected version: %s\n' "$destination" "$installed_version" >&2
	exit 1
fi
printf '%s\n' "$destination"
