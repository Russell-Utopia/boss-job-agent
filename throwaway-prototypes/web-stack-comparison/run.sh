#!/usr/bin/env bash
set -euo pipefail

prototype_root="$(cd "$(dirname "$0")" && pwd)"

cd "$prototype_root/react"
if [[ ! -d node_modules ]]; then
  npm install --no-audit --no-fund
fi
npm run build

cd "$prototype_root"
exec go run . -root "$prototype_root"
