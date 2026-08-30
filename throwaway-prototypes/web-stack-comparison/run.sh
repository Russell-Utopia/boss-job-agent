#!/usr/bin/env bash
set -euo pipefail

prototype_root="$(cd "$(dirname "$0")" && pwd)"
prototype_toolchain="go$(awk '$1 == "go" { print $2 }' "$prototype_root/go.mod")"

cd "$prototype_root/react"
if [[ ! -d node_modules ]]; then
  npm install --no-audit --no-fund
fi
npm run build

cd "$prototype_root"
exec env GOTOOLCHAIN="$prototype_toolchain" go run . -root "$prototype_root"
