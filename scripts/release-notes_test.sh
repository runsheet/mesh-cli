#!/usr/bin/env bash
# Builds a temp repo with a squash-merge history and a fixture PRs.json (so
# this needs no network) and asserts scripts/release-notes.sh groups them
# by goal slug in merge order.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

repo="$work/repo"
mkdir -p "$repo"
cd "$repo"
git init -q
git config user.email test@example.com
git config user.name "Test"

git commit -q --allow-empty -m "initial"
git tag v0.1.0

git commit -q --allow-empty -m "Goal A: item (#249)"
git commit -q --allow-empty -m "Goal A: item 2 (#254)"
git commit -q --allow-empty -m "harness: trim (#265)"
no_pr_sha="$(git commit -q --allow-empty -m "docs: fix a typo" && git rev-parse HEAD)"
git tag v0.1.1

cat >"$work/prs.json" <<'EOF'
[
  {"number": 249, "title": "Goal A: item", "headRefName": "mesh/tools-show-their-use-1", "url": "https://github.com/runsheet/mesh-cli/pull/249"},
  {"number": 254, "title": "Goal A: item 2", "headRefName": "mesh/tools-show-their-use-2", "url": "https://github.com/runsheet/mesh-cli/pull/254"},
  {"number": 265, "title": "harness: trim tool results", "headRefName": "feature/trim", "url": "https://github.com/runsheet/mesh-cli/pull/265"}
]
EOF

out="$(RELEASE_NOTES_PRS="$work/prs.json" "$repo_root/scripts/release-notes.sh" v0.1.0 v0.1.1)"

echo "$out"

fail() {
  echo "FAIL: $1" >&2
  exit 1
}

grep -q "^### tools-show-their-use\$" <<<"$out" || fail "missing ### tools-show-their-use section"
grep -q "^Goal doc: https://github.com/runsheet/mesh-cli/blob/main/specs/goal-tools-show-their-use.md\$" <<<"$out" \
  || fail "missing goal-doc link"
grep -q "^- Goal A: item (#249)\$" <<<"$out" || fail "missing PR #249 bullet"
grep -q "^- Goal A: item 2 (#254)\$" <<<"$out" || fail "missing PR #254 bullet"

first_249="$(grep -n "^- Goal A: item (#249)\$" <<<"$out" | head -1 | cut -d: -f1)"
first_254="$(grep -n "^- Goal A: item 2 (#254)\$" <<<"$out" | head -1 | cut -d: -f1)"
[ "$first_249" -lt "$first_254" ] || fail "PR bullets not in merge order"

grep -q "^### Other\$" <<<"$out" || fail "missing ### Other section"
grep -q "^- harness: trim tool results (#265)\$" <<<"$out" || fail "missing #265 under Other"

grep -q "^### Commits\$" <<<"$out" || fail "missing ### Commits section"
short="$(git rev-parse --short "$no_pr_sha")"
grep -q "^- docs: fix a typo (${short})\$" <<<"$out" || fail "missing PR-less commit"

echo "ok"
