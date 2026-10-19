#!/usr/bin/env bash
# Lists the stale item checkouts under a work root: a checkout whose
# `.git` file names a `gitdir:` that no longer exists — its shared
# clone's .git/worktrees/<n> removed by a prune, or the clone gone — so
# every git command in it fails `not a git repository` (goal
# a-checkout-whose-worktree-is-gone-is-made-again; ADR-028, One worktree
# per item). The node makes such a checkout again when its item is next
# prepared (Git.RebuildWorktree, removeStale); this is the reading the
# goal's after-release check takes, and it should print nothing.
#
# It is the goal's find, with one change: a checkout is
# <work root>/<goal>/<seq>, so its .git is at depth 3, where the goal's
# `-mindepth 2 -maxdepth 2` saw only depth 2. A relative gitdir is read
# against the checkout. A measurement (ADR-020): it exits 0 and prints
# one line per stale checkout, `<checkout> -> <gitdir>`, never a
# verdict.
#
# usage: scripts/stale-worktrees.sh [<work root>]   (default ~/.mesh/work)
set -uo pipefail

root="${1:-$HOME/.mesh/work}"
[ -d "$root" ] || exit 0

find "$root" -mindepth 2 -maxdepth 3 -name .git -type f 2>/dev/null | sort |
  while IFS= read -r f; do
    g=$(sed -n 's/^gitdir: //p' "$f")
    [ -n "$g" ] || continue
    d=$(dirname "$f")
    case "$g" in
      /*) p="$g" ;;
      *) p="$d/$g" ;;
    esac
    [ -e "$p" ] || echo "$d -> $g"
  done
exit 0
