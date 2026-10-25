#!/usr/bin/env bash
# contracts pull: copies runsheet's published contract fixtures
# (runsheet specs/contracts/*.json, written by its e2e TestContracts;
# runsheet ADR 026, ADR 069) into specs/contracts/runsheet/, byte for
# byte, and records the ref and commit they came from in
# specs/contracts/runsheet/PULLED. The gatewaytest fakes check every
# request they take and every answer they give on the routes they fake
# against these copies (internal/gateway/gatewaytest/contracts.go), so a
# fake that speaks a shape runsheet does not publish fails its own test.
#
#   scripts/contracts-pull.sh            # runsheet main
#   scripts/contracts-pull.sh <ref>      # a branch, tag or commit
#
# CONTRACTS_REPO overrides where runsheet is read from and CONTRACTS_DEST
# where the copies go (scripts/contracts_pull_test.go points them at a
# local repository and a scratch directory). Reading runsheet/runsheet needs git credentials
# for it (gh auth setup-git). Commit the diff: a change here is a change
# to what the fakes accept.
set -euo pipefail
cd "$(dirname "$0")/.."

ref=${1:-main}
repo=${CONTRACTS_REPO:-https://github.com/runsheet/runsheet.git}
dest=${CONTRACTS_DEST:-specs/contracts/runsheet}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

git -C "$work" init -q
git -C "$work" remote add origin "$repo"
git -C "$work" fetch -q --depth 1 --filter=blob:none origin "$ref" 2>"$work/fetch.err" || {
  cat "$work/fetch.err" >&2
  echo "contracts pull: cannot fetch $ref from $repo" >&2
  exit 1
}
git -C "$work" sparse-checkout set --no-cone 'specs/contracts/*.json' 2>/dev/null
git -C "$work" -c advice.detachedHead=false checkout -q FETCH_HEAD
commit=$(git -C "$work" rev-parse FETCH_HEAD)

shopt -s nullglob
pulled=("$work"/specs/contracts/*.json)
if [ ${#pulled[@]} -eq 0 ]; then
  echo "contracts pull: $ref ($commit) has no specs/contracts/*.json" >&2
  exit 1
fi

mkdir -p "$dest"
rm -f "$dest"/*.json
cp "${pulled[@]}" "$dest"/
printf 'ref %s\ncommit %s\n' "$ref" "$commit" >"$dest/PULLED"

echo "contracts pull: ${#pulled[@]} fixture(s) from runsheet $ref ($commit) into $dest"
