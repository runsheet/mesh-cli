#!/usr/bin/env bash
# Builds temp git repos (no network: RELEASE_MIRROR is a local repo or
# empty, and gh is a fake on PATH) and asserts scripts/release-on-merge.sh
# picks the next tag, or none, for each case.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
script="$repo_root/scripts/release-on-merge.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# No PR lookups (and so no network) unless a case names the repo and
# puts its fake gh first on PATH.
export GITHUB_REPOSITORY=""
export RELEASE_MIRROR=""

fail() {
  echo "FAIL: $1" >&2
  exit 1
}

# new_repo <name>: an empty repo with one initial node-changing commit.
new_repo() {
  local dir="$work/$1"
  mkdir -p "$dir"
  git -C "$dir" init -q
  git -C "$dir" config user.email test@example.com
  git -C "$dir" config user.name Test
  mkdir -p "$dir/internal"
  echo "package x" >"$dir/internal/x.go"
  git -C "$dir" add -A
  git -C "$dir" commit -q -m "initial"
  echo "$dir"
}

# commit <dir> <path> <subject>: change <path> and commit it.
commit() {
  mkdir -p "$(dirname "$1/$2")"
  echo "$RANDOM $3" >>"$1/$2"
  git -C "$1" add -A
  git -C "$1" commit -q -m "$3"
  git -C "$1" rev-parse HEAD
}

# expect <case> <want> <out>
expect() {
  echo "$1: $3"
  [ "$3" = "$2" ] || fail "$1: want '$2', got '$3'"
}

# 1. Patch: v0.1.0, then a change under internal/.
r="$(new_repo patch)"
git -C "$r" tag v0.1.0
sha="$(commit "$r" internal/node/loop.go "node: a fix (#401)")"
out="$("$script" "$r" "$sha" 2>/dev/null)"
expect "patch v0.1.0 -> v0.1.1" "tag=v0.1.1" "$out"

# 2. Only test prereleases: the first release is v0.1.0.
r="$(new_repo prerelease)"
git -C "$r" tag v0.0.0-runsheet-test.6
git -C "$r" tag v0.0.0-selfupdate-test.1
sha="$(commit "$r" cmd/mesh/main.go "cmd: a flag (#402)")"
out="$("$script" "$r" "$sha" 2>/dev/null)"
expect "only test prereleases -> v0.1.0" "tag=v0.1.0" "$out"

# 3. Minor by the merged PR's label, through a fake gh.
mkdir -p "$work/fakegh"
cat >"$work/fakegh/gh" <<'EOF'
#!/usr/bin/env bash
# The fake answers the commit's PR: #403, labelled release:minor.
case "$*" in
  *"/pulls --jq .[0].labels[].name") printf 'goal\nrelease:minor\n' ;;
  *"/pulls --jq .[0].number") echo 403 ;;
  *) echo "fake gh: unexpected $*" >&2; exit 1 ;;
esac
EOF
chmod +x "$work/fakegh/gh"
r="$(new_repo label)"
git -C "$r" tag v0.1.0
git -C "$r" tag v0.1.4
sha="$(commit "$r" internal/harness/prompt.go "harness: a new prompt (#403)")"
out="$(GITHUB_REPOSITORY=runsheet/mesh-cli PATH="$work/fakegh:$PATH" "$script" "$r" "$sha" 2>/dev/null)"
expect "release:minor label -> minor" "tag=v0.2.0" "$out"

# 4. Minor by the goal doc's frontmatter.
r="$(new_repo goaldoc)"
git -C "$r" tag v0.1.1
mkdir -p "$r/specs"
cat >"$r/specs/goal-big-change.md" <<'EOF'
---
title: Goal — a big change
release: minor
---

# Goal
EOF
echo "package y" >"$r/internal/y.go"
git -C "$r" add -A
git -C "$r" commit -q -m "Goal big-change: item 1 (#404)"
sha="$(git -C "$r" rev-parse HEAD)"
out="$("$script" "$r" "$sha" 2>/dev/null)"
expect "goal doc release: minor -> minor" "tag=v0.2.0" "$out"

# 4b. "release: minor" in the body, not the frontmatter, is a patch.
cat >"$r/specs/goal-small.md" <<'EOF'
---
title: Goal — small
---

release: minor
EOF
echo "// more" >>"$r/internal/y.go"
git -C "$r" add -A
git -C "$r" commit -q -m "Goal small: item 1 (#405)"
sha="$(git -C "$r" rev-parse HEAD)"
out="$("$script" "$r" "$sha" 2>/dev/null)"
expect "release: minor outside the frontmatter -> patch" "tag=v0.1.2" "$out"

# 5. Docs-only: refused by the script's own path check.
r="$(new_repo docs)"
git -C "$r" tag v0.1.0
commit "$r" docs/adr/adr-099.md "docs: an ADR" >/dev/null
commit "$r" specs/goal-x.md "specs: a goal" >/dev/null
sha="$(commit "$r" README.md "README: a line (#406)")"
out="$("$script" "$r" "$sha" 2>/dev/null)"
echo "docs-only: $out"
case "$out" in
  "no release: "*) ;;
  *) fail "docs-only: want 'no release: …', got '$out'" ;;
esac
sha="$(commit "$r" .github/workflows/x.yml "ci: a workflow")"
out="$("$script" "$r" "$sha" --dry-run 2>/dev/null)"
echo "workflow-only (dry run): $out"
case "$out" in
  "no release: "*) ;;
  *) fail "workflow-only: want 'no release: …', got '$out'" ;;
esac

# 6. A merge commit's changes are against its first parent.
r="$(new_repo merge)"
git -C "$r" tag v0.1.0
git -C "$r" checkout -q -b feature
commit "$r" internal/z.go "internal: z" >/dev/null
git -C "$r" checkout -q -
git -C "$r" merge -q --no-ff -m "Merge pull request #407" feature
sha="$(git -C "$r" rev-parse HEAD)"
out="$("$script" "$r" "$sha" 2>/dev/null)"
expect "merge commit -> patch" "tag=v0.1.1" "$out"

# 7. The mirror's v0.2.x line counts; this repo's lower tags do not win.
m="$(new_repo mirror)"
git -C "$m" tag v0.2.186
r="$(new_repo withmirror)"
git -C "$r" tag v0.1.0
sha="$(commit "$r" go.mod "go.mod: a bump (#408)")"
out="$(RELEASE_MIRROR="$m" "$script" "$r" "$sha" 2>/dev/null)"
expect "mirror v0.2.186 -> v0.2.187" "tag=v0.2.187" "$out"

# 8. Already tagged by hand: no second tag.
git -C "$r" tag v0.2.187 "$sha"
out="$("$script" "$r" "$sha" 2>/dev/null)"
expect "already tagged" "no release: ${sha:0:7} is already tagged v0.2.187" "$out"

echo "PASS: release-on-merge"
