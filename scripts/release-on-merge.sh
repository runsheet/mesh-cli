#!/usr/bin/env bash
# release-on-merge.sh <repo-dir> <sha> [--dry-run]
#
# release-on-merge.yml calls this on every push to main that touches the
# node (docs/adr/adr-002-signed-tag-releases.md, "Amendment: every
# node-changing merge is a release"). It decides whether <sha> is a
# release and, if so, which tag: it prints exactly one line on stdout,
#
#   tag=vX.Y.Z             the workflow tags <sha> with it and pushes it
#   no release: <why>      the workflow does nothing
#
# and says how it got there on stderr. It never tags or pushes itself.
#
# - Node-changing is a path under cmd/, internal/, images/node/, or
#   go.mod / go.sum, among the files <sha> changes against its first
#   parent. The workflow's `paths` filter says the same; this check is
#   the script's own, so a direct call on a docs-only commit is refused.
# - The last release is the highest tag matching ^v[0-9]+\.[0-9]+\.[0-9]+$
#   in <repo-dir> (after fetching tags from origin, when there is one)
#   and on the public mirror (RELEASE_MIRROR, default
#   https://github.com/runsheet/mesh.git; empty skips it), since the
#   mirror carries the public v0.2.x line. Tags with a "-" are test
#   prereleases and never count. None at all means v0.1.0.
# - The bump is the patch, or the minor (patch 0) when the merged PR
#   carries the label release:minor, or a specs/goal-*.md the commit
#   changes has `release: minor` in its frontmatter.
# - The merged PR is `gh api repos/$GITHUB_REPOSITORY/commits/<sha>/pulls`,
#   falling back to the squash subject's "(#NNN)". Without gh or
#   GITHUB_REPOSITORY the label is not read (said on stderr).
# - <sha> already carrying a plain vX.Y.Z tag is no release (a re-run,
#   or a tag cut by hand).
#
# --dry-run changes nothing the script decides; it only says on stderr
# that the caller will push nothing.
set -euo pipefail

usage() {
  echo "usage: $0 <repo-dir> <sha> [--dry-run]" >&2
  exit 2
}

dry_run=false
args=()
for a in "$@"; do
  case "$a" in
    --dry-run) dry_run=true ;;
    -*) usage ;;
    *) args+=("$a") ;;
  esac
done
[ "${#args[@]}" -eq 2 ] || usage
repo_dir="${args[0]}"
sha_arg="${args[1]}"
mirror="${RELEASE_MIRROR-https://github.com/runsheet/mesh.git}"

say() { echo "release-on-merge: $*" >&2; }
plain='^v[0-9]+\.[0-9]+\.[0-9]+$'

cd "$repo_dir"
sha="$(git rev-parse --verify "${sha_arg}^{commit}")"

# The files the merge changes, against its first parent.
if git rev-parse --verify -q "${sha}^1" >/dev/null; then
  files="$(git diff --name-only "${sha}^1" "$sha")"
else
  files="$(git diff-tree --no-commit-id --name-only -r --root "$sha")"
fi

node_files="$(grep -E '^(cmd/|internal/|images/node/|go\.mod$|go\.sum$)' <<<"$files" || true)"
if [ -z "$node_files" ]; then
  $dry_run && say "dry run: nothing is tagged or pushed"
  echo "no release: ${sha:0:7} changes nothing under cmd/, internal/, images/node/, go.mod or go.sum"
  exit 0
fi
say "node-changing: $(head -3 <<<"$node_files" | paste -sd' ' -)$([ "$(wc -l <<<"$node_files")" -gt 3 ] && echo ' …')"

if git remote get-url origin >/dev/null 2>&1; then
  git fetch -q --tags --force origin || say "could not fetch tags from origin; using the local ones"
fi

already="$(git tag --points-at "$sha" | grep -E "$plain" | sort -V | tail -1 || true)"
if [ -n "$already" ]; then
  $dry_run && say "dry run: nothing is tagged or pushed"
  echo "no release: ${sha:0:7} is already tagged $already"
  exit 0
fi

tags="$(git tag --list)"
if [ -n "$mirror" ]; then
  if mirror_tags="$(git ls-remote --tags --refs "$mirror" 2>/dev/null)"; then
    tags="$tags"$'\n'"$(sed 's#.*refs/tags/##' <<<"$mirror_tags")"
  else
    say "could not list the mirror's tags ($mirror); using this repo's alone"
  fi
fi
last="$(grep -E "$plain" <<<"$tags" | sort -V | tail -1 || true)"

# Minor, by the PR's label or the goal doc's frontmatter.
minor=""
pr=""
if [ -n "${GITHUB_REPOSITORY:-}" ] && command -v gh >/dev/null 2>&1; then
  labels="$(gh api "repos/$GITHUB_REPOSITORY/commits/$sha/pulls" --jq '.[0].labels[].name' 2>/dev/null || true)"
  pr="$(gh api "repos/$GITHUB_REPOSITORY/commits/$sha/pulls" --jq '.[0].number' 2>/dev/null || true)"
  if [ -z "$pr" ]; then
    pr="$(git log -1 --format=%s "$sha" | grep -oE '\(#[0-9]+\)$' | tr -dc '0-9' || true)"
    if [ -n "$pr" ]; then
      labels="$(gh api "repos/$GITHUB_REPOSITORY/issues/$pr/labels" --jq '.[].name' 2>/dev/null || true)"
    fi
  fi
  if grep -qx 'release:minor' <<<"$labels"; then
    minor="the label release:minor on #$pr"
  fi
else
  say "no gh or GITHUB_REPOSITORY: the PR's labels are not read"
fi

if [ -z "$minor" ]; then
  while IFS= read -r doc; do
    [ -n "$doc" ] || continue
    # The frontmatter: the lines between the first two "---".
    if git show "$sha:$doc" 2>/dev/null \
      | awk 'NR==1 && $0!="---"{exit} NR>1 && $0=="---"{exit} NR>1{print}' \
      | grep -qE '^release:[[:space:]]*"?minor"?[[:space:]]*$'; then
      minor="release: minor in $doc"
      break
    fi
  done < <(grep -E '^specs/goal-[^/]*\.md$' <<<"$files" || true)
fi

if [ -z "$last" ]; then
  next="v0.1.0"
  say "no vX.Y.Z release yet: the first is $next"
else
  IFS=. read -r major min patch <<<"${last#v}"
  if [ -n "$minor" ]; then
    next="v${major}.$((min + 1)).0"
    say "minor: $minor ($last → $next)"
  else
    next="v${major}.${min}.$((patch + 1))"
    say "patch ($last → $next)"
  fi
fi

$dry_run && say "dry run: nothing is tagged or pushed"
echo "tag=$next"
