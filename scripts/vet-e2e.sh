#!/usr/bin/env bash
# vet-e2e.sh — go vet under the e2e build tag, then go build.
#
# internal/sandbox's e2e-tagged files (localimage_e2e_test.go) are
# invisible to a plain `go vet ./...`; this is what sees them.
set -euo pipefail

cd "$(dirname "$0")/.."

echo "== go vet -tags e2e ./internal/sandbox/"
go vet -tags e2e ./internal/sandbox/

echo "== go build ./..."
go build ./...
