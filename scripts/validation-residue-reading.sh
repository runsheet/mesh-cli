#!/usr/bin/env bash
# Takes the reading the goal A node validates from a served diff with a
# shared cache owes (item 5's acceptance, item 2's SharedCache): over a
# window (default 60 minutes), does validation leave anything behind
# under /tmp or ~/.mesh/work owned by the node users, and what does
# `mesh node status`'s `validation cache:` line (nodecmd.go's
# cacheStatusLine, item 5) read at either end. Like
# host-backoff-reading.sh and evaluate-once-reading.sh, this is a
# measurement (ADR-020's `Measure:` item): it exits 0 and prints what it
# found for a human to read, never a verdict a build is judged on.
#
# Reading taken 2026-10-15 22:13 UTC on
# mesh-a-node-validates-from-a-served-diff-with-a1515f5f-0 (this item's
# build/evaluation container, not dl360 — the goal's own after-release
# hour on dl360 is the goal's acceptance, not this item's gate): with no
# node running here and no ~/.mesh/work entries, `./scripts/validation-residue-reading.sh --window 0`
# read:
#   == before
#   entries: 5
#   bytes: 18810001
#   validation cache: no node running here
#   == after
#   entries: 5
#   bytes: 18810077
#   validation cache: no node running here
#   == difference
#   new entries: 0
#   new bytes: 76
#   validations in window: 0
# (the 76 bytes is the container's own /tmp churning between the two
# instants, not the scan's doing: scan() now keeps its file list in
# memory, never writing a scratch file under /tmp, so scanning /tmp no
# longer counts itself.)
#
# Sourceable: this file's top-level run is behind the
# `[[ "${BASH_SOURCE[0]}" == "$0" ]]` guard at the bottom, so
# scripts/validation-residue-reading_test.sh can `source` it and call
# count_validations directly against a fixture node.log, which --dry
# has no way to reach (its window is the whole fixture file).
set -euo pipefail

usage() {
  cat <<'EOF'
usage: scripts/validation-residue-reading.sh [--window MINUTES] [--users U[,U...]] [--log PATH] [--help]
       scripts/validation-residue-reading.sh --dry FIXTURE-DIR

Snapshots the regular files and their total bytes under /tmp and
$MESH_HOME/work (default $MESH_HOME ~/.mesh) owned by the node users,
waits --window minutes (default 60), snapshots again, and prints the
difference — new entries, new bytes — so an hour of validation running
on the node can be read as leaving nothing behind (the goal's own
acceptance).

--users is a comma- or space-separated list of user names; default,
the distinct owners of $MESH_HOME/work's top-level entries (one node's
goal checkouts), else the user running this script when there are
none.

--log is the node.log read for the validations the window records
(default $MESH_HOME/node.log): lines logging `attestation recorded `
(internal/node/validation.go's vr.logf, after a push's validation,
rebase's and a merge batch's alike) between the before and after
snapshots.

Both ends also print `mesh node status`'s own `validation cache:` line
($MESH_BIN, default `mesh`; $STATUS_TIMEOUT, default 60s) — the shared
tree cache's hit rate, size and cap (internal/commands/nodecmd.go's
cacheStatusLine) — unchanged by this script; a status call that fails
or times out prints why instead.

--dry FIXTURE-DIR runs over a fixture instead of live state, taking no
time and reading no real node: FIXTURE-DIR/before and FIXTURE-DIR/after
are scanned for every regular file under them (no user filter — the
fixture is already the two snapshots), FIXTURE-DIR/status-before.txt
and FIXTURE-DIR/status-after.txt stand in for `mesh node status`'s
output (its `validation cache:` line is read from each the same way),
FIXTURE-DIR/node.log stands in for the window's node.log — every
`attestation recorded ` line in it counts, the fixture already being
scoped to the window — and FIXTURE-DIR/work stands in for
$MESH_HOME/work.

Both runs close with `leftover work directories: N`: the directories
under $MESH_HOME/work/<goal>/<seq> with no live job (no
`<seq>.session/state.json` beside them whose pid is alive or 0). Every
job works under $MESH_HOME/jobs/<id>/ (ADR-039), so a node reads 0.

Then `shared-clone worktrees: N`: entries of `git worktree list`
beyond the clone itself, summed over every clone under
$MESH_HOME/repos (`Git.ReposRoot`, `<owner>/<name>`) — live, `git -C <clone> worktree list --porcelain`
run directly on each clone found by its `.git` directory; under --dry,
each clone's `worktree-list.txt` (one line per entry the real command
would print, the clone's own always first) stands in for that same
output, so the fixture takes no git repository at all. A clone with
only its own entry, or no clones at all, counts 0.

The fixture this repository carries is read by

  bash scripts/validation-residue-reading.sh --dry scripts/testdata/validation-residue-reading

which prints the `== before` and `== after` entries and bytes, each
end's `validation cache:` line, and the `== difference` between them.
EOF
}

# cache_line [status-file]: `mesh node status`'s own `validation cache:`
# line, read live (STATUS_TIMEOUT seconds, default 60) or, given a
# file, from it — either way the first matching line, or why there is
# none.
cache_line() {
  local out rc
  if [ $# -ge 1 ]; then
    if [ -f "$1" ]; then
      out="$(grep -m1 '^validation cache:' "$1" || true)"
      if [ -n "$out" ]; then
        printf '%s\n' "$out"
      else
        echo "validation cache: not in $1"
      fi
    else
      echo "validation cache: fixture not found: $1"
    fi
    return 0
  fi
  if ! command -v "$mesh_bin" >/dev/null 2>&1; then
    echo "validation cache: no node running here"
    return 0
  fi
  if out="$(timeout "${STATUS_TIMEOUT:-60}" "$mesh_bin" node status 2>&1)"; then
    rc=0
  else
    rc=$?
  fi
  out="$(grep -m1 '^validation cache:' <<<"$out" || true)"
  if [ -n "$out" ]; then
    printf '%s\n' "$out"
  elif [ "$rc" -eq 124 ]; then
    echo "validation cache: mesh node status timed out after ${STATUS_TIMEOUT:-60}s"
  else
    echo "validation cache: mesh node status printed no validation cache line"
  fi
}

# default_users: the distinct owners of $home/work's top-level entries
# (one per node goal in progress), else the user running this script.
default_users() {
  if [ -d "$home/work" ]; then
    find "$home/work" -mindepth 1 -maxdepth 1 -printf '%u\n' 2>/dev/null | sort -u
  fi
}

# scan root...: prints "<entries> <bytes>" for every regular file found
# under the given roots (existing ones only), filtered to $scan_users
# when that array is non-empty. Kept entirely in memory (no scratch
# file under /tmp) so scanning /tmp never counts the scan's own state.
scan_users=()
scan() {
  local count bytes root existing=()
  for root in "$@"; do
    [ -d "$root" ] && existing+=("$root")
  done
  if [ ${#existing[@]} -eq 0 ]; then
    echo "0 0"
    return 0
  fi
  local -a files=()
  if [ ${#scan_users[@]} -eq 0 ]; then
    readarray -d '' files < <(find "${existing[@]}" -xdev -type f -print0 2>/dev/null | sort -z -u)
  else
    local u
    readarray -d '' files < <(
      for u in "${scan_users[@]}"; do
        find "${existing[@]}" -xdev -type f -user "$u" -print0 2>/dev/null
      done | sort -z -u
    )
  fi
  count=${#files[@]}
  bytes=0
  if [ "$count" -gt 0 ]; then
    bytes="$(printf '%s\0' "${files[@]}" | xargs -0 -r stat -c '%s' | awk '{s+=$1} END{print s+0}')"
  fi
  printf '%s %s\n' "$count" "$bytes"
}

# leftover_work_dirs root: how many work directories under
# root/<goal>/<seq> (ADR-039 left ~/.mesh/work behind: every job works
# in ~/.mesh/jobs/<id>/) have no live job — no `<seq>.session/state.json`
# beside them (sessionunit's State, written while a session runs) whose
# pid is alive or 0 (a sandboxed session's, judged by its container).
# The `.session` and `.hooks` dirs themselves, and dot dirs, are not
# work directories. A missing root gives 0.
leftover_work_dirs() {
  local root="$1" dir state pid n=0
  [ -d "$root" ] || { echo 0; return 0; }
  while IFS= read -r -d '' dir; do
    case "$(basename "$dir")" in .* | *.session | *.hooks) continue ;; esac
    state="$dir.session/state.json"
    if [ -f "$state" ]; then
      pid="$(sed -n 's/.*"pid"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$state" | head -n1)"
      pid="${pid:-0}"
      if [ "$pid" -eq 0 ] || kill -0 "$pid" 2>/dev/null; then
        continue
      fi
    fi
    n=$((n + 1))
  done < <(find "$root" -mindepth 2 -maxdepth 2 -type d -print0 2>/dev/null)
  echo "$n"
}

# shared_clone_worktrees root: entries of `git worktree list` beyond
# the clone itself (its own first entry, never counted), summed over
# every shared clone under root — a node's builds and rebases that
# register a worktree on a shared clone instead of cloning their own
# (the goal this item measures towards; mesh ADR-058 §6, ADR-039). A
# live clone is found by its `.git` directory and read with `git -C
# <clone> worktree list --porcelain`'s `^worktree ` lines; a fixture
# clone (--dry, no real git repository at all) is found by its
# `worktree-list.txt`, read the same way — one entry per non-empty
# line, so a fixture can stand in for the real command's output
# byte-for-byte. A missing root, a clone with only its own entry, or
# one git cannot read, counts 0 for it; never a `git worktree prune`,
# a read only.
shared_clone_worktrees() {
  local root="$1" n=0 dir f count
  [ -d "$root" ] || { echo 0; return 0; }
  while IFS= read -r -d '' dir; do
    dir="$(dirname "$dir")"
    count="$(git -C "$dir" worktree list --porcelain 2>/dev/null | grep -c '^worktree ' || true)"
    count="${count:-0}"
    [ "$count" -gt 0 ] && n=$((n + count - 1))
  done < <(find "$root" -type d -name '.git' -print0 2>/dev/null)
  while IFS= read -r -d '' f; do
    count="$(grep -c '[^[:space:]]' "$f" || true)"
    count="${count:-0}"
    [ "$count" -gt 0 ] && n=$((n + count - 1))
  done < <(find "$root" -type f -name 'worktree-list.txt' -print0 2>/dev/null)
  echo "$n"
}

# count_validations log [since] [until]: how many `attestation recorded
# ` lines (internal/node/validation.go) fall on or between the given
# node.log timestamps (its own `YYYY/MM/DD HH:MM:SS`, fixed-width); with
# neither bound, every one in the file.
count_validations() {
  local f="$1" since="${2:-}" until_="${3:-}"
  [ -f "$f" ] || { echo 0; return 0; }
  LC_ALL=C awk -v since="$since" -v until="$until_" '
    {
      if (substr($0, 1, 11) != "mesh node: ") next
      ts = substr($0, 12, 19)
      rest = substr($0, 32)
      if (since != "" && ts < since) next
      if (until != "" && ts > until) next
      if (index(rest, "attestation recorded ") > 0) n++
    }
    END { print n + 0 }
  ' "$f"
}

# main args...: the script's own run — argument parsing, the --dry
# fixture path and the live snapshot-wait-snapshot path. Kept out of
# top level (behind the BASH_SOURCE guard below) so the test can
# source this file for its functions alone, with no node.log waited on
# and no real /tmp scanned.
main() {
  window=60
  users_arg=""
  log=""
  dry_dir=""

  while [ $# -gt 0 ]; do
    case "$1" in
      --help | -h) usage; exit 0 ;;
      --window) window="$2"; shift 2 ;;
      --window=*) window="${1#--window=}"; shift ;;
      --users) users_arg="$2"; shift 2 ;;
      --users=*) users_arg="${1#--users=}"; shift ;;
      --log) log="$2"; shift 2 ;;
      --log=*) log="${1#--log=}"; shift ;;
      --dry) dry_dir="$2"; shift 2 ;;
      --dry=*) dry_dir="${1#--dry=}"; shift ;;
      -*) echo "validation-residue-reading: unknown argument: $1" >&2; usage >&2; exit 2 ;;
      *) echo "validation-residue-reading: unexpected argument: $1" >&2; usage >&2; exit 2 ;;
    esac
  done

  case "$window" in '' | *[!0-9]*) echo "validation-residue-reading: --window wants a whole number of minutes, got '$window'" >&2; exit 2 ;; esac

  home="${MESH_HOME:-$HOME/.mesh}"
  mesh_bin="${MESH_BIN:-mesh}"
  log="${log:-$home/node.log}"

  if [ -n "$dry_dir" ]; then
    if [ ! -d "$dry_dir" ]; then
      echo "validation-residue-reading: --dry directory not found: $dry_dir" >&2
      exit 2
    fi
    echo "== validation-residue-reading (dry run) taken $(date '+%Y-%m-%d %H:%M:%S %z') on $(hostname 2>/dev/null || uname -n), fixture $dry_dir"

    scan_users=()
    read -r before_count before_bytes < <(scan "$dry_dir/before")
    echo "== before"
    echo "entries: $before_count"
    echo "bytes: $before_bytes"
    cache_line "$dry_dir/status-before.txt"

    read -r after_count after_bytes < <(scan "$dry_dir/after")
    echo "== after"
    echo "entries: $after_count"
    echo "bytes: $after_bytes"
    cache_line "$dry_dir/status-after.txt"

    echo "== difference"
    echo "new entries: $((after_count - before_count))"
    echo "new bytes: $((after_bytes - before_bytes))"
    echo "validations in window: $(count_validations "$dry_dir/node.log")"
    echo "leftover work directories: $(leftover_work_dirs "$dry_dir/work")"
    echo "shared-clone worktrees: $(shared_clone_worktrees "$dry_dir/repos")"
    exit 0
  fi

  users=()
  if [ -n "$users_arg" ]; then
    IFS=', ' read -r -a users <<<"$users_arg"
  else
    while IFS= read -r u; do
      [ -n "$u" ] && users+=("$u")
    done < <(default_users)
  fi
  if [ ${#users[@]} -eq 0 ]; then
    users=("$(id -un)")
  fi

  echo "== validation-residue-reading taken $(date '+%Y-%m-%d %H:%M:%S %z') on $(hostname 2>/dev/null || uname -n), window ${window}m, users: ${users[*]}"

  scan_users=("${users[@]}")
  start_ts="$(date '+%Y/%m/%d %H:%M:%S')"
  read -r before_count before_bytes < <(scan /tmp "$home/work")
  echo "== before"
  echo "entries: $before_count"
  echo "bytes: $before_bytes"
  cache_line

  if [ "$window" -gt 0 ]; then
    echo "validation-residue-reading: waiting ${window}m ..." >&2
    sleep "$((window * 60))"
  fi

  end_ts="$(date '+%Y/%m/%d %H:%M:%S')"
  read -r after_count after_bytes < <(scan /tmp "$home/work")
  echo "== after"
  echo "entries: $after_count"
  echo "bytes: $after_bytes"
  cache_line

  echo "== difference"
  echo "new entries: $((after_count - before_count))"
  echo "new bytes: $((after_bytes - before_bytes))"
  echo "validations in window: $(count_validations "$log" "$start_ts" "$end_ts")"
  echo "leftover work directories: $(leftover_work_dirs "$home/work")"
  echo "shared-clone worktrees: $(shared_clone_worktrees "$home/repos")"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
