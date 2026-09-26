#!/usr/bin/env bash
# Fails when internal/gateway/fields_gen.go is not what the generator writes
# from the committed schema.json and fields/*.txt (docs/adr/adr-010). Never
# reads the live gateway, so the check is deterministic.
set -euo pipefail
cd "$(dirname "$0")/.."

f=internal/gateway/fields_gen.go
before=$(mktemp)
trap 'rm -f "$before"' EXIT
cp "$f" "$before"

go generate ./internal/gateway/...

# Compare with the file as it was (a hand edit the generator just overwrote
# is not a diff against the index) and with the committed one.
if ! cmp -s "$before" "$f" || ! git diff --exit-code -- "$f"; then
  diff -u "$before" "$f" || true
  echo "fields_gen.go is stale: run go generate ./internal/gateway/..." >&2
  exit 1
fi
