#!/usr/bin/env bash
# Takes the reading item 5 of a-host-sessions-scratch-dies-with-the-session
# owes: what the node users hold under /tmp, summed by name prefix. The
# goal's evidence is dl360's 2026-10-15 `du -xsh /tmp/*` by prefix
# (runsheet-* 67G, latchkey-* 42G, mesh-home-* 18G, go-build* 13G,
# mesh-eval* 13G) and its after-release check — "an hour of host
# validations and evaluations on dl360 adds nothing under /tmp owned
# by the node users" — is this reading taken twice, an hour apart, the
# second with --baseline naming the first.
#
# Like mesh-home-reading.sh and evaluate-once-reading.sh, this is a
# measurement (ADR-020's `Measure:` item): it exits 0 and prints what
# /tmp holds for a human to read, never a verdict a build is judged on.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: scripts/host-tmp-reading.sh [--baseline FILE] <user>...

For every top-level entry under $TMPDIR_ROOT (default /tmp) owned by
one of the named users (dl360's node users: anythink grapevine), sums
its size (`du -sb`, apparent bytes) by name prefix, in this order, the
first that matches winning:

  runsheet-*  latchkey-*  mesh-home-*  mesh-eval*  mesh-session-*
  go-build*   other

and prints:

  == /tmp by user and prefix
    one line per user and prefix, every prefix listed, zeros included:
      <user>	<prefix>	bytes=<n>	entries=<n>
    then the users summed, as user `all`:
      all	<prefix>	bytes=<n>	entries=<n>
    and the grand total:
      total	bytes=<n>	entries=<n>

With --baseline FILE (a previous run's output, saved), it also prints:

  == since baseline
    one line per user and prefix whose bytes or entries changed:
      <user>	<prefix>	bytes=<+n|-n>	entries=<+n|-n>
    or `(no change)`, then:
      added	bytes=<+n|-n>	entries=<+n|-n>

and the verdict, `unchanged` when the total is the baseline's, else
`grew` or `shrank`. A row the baseline lacks counts from zero.

$TMPDIR_ROOT is overridable so a test can read a fixture directory;
$STAT (default `stat`) is the command asked an entry's owner as
`$STAT -c %U <path>`, split on spaces (`bash /path/fake-stat` runs a
script from a dir mounted noexec), overridable so a test can name two
users without root.
EOF
}

baseline=""
users=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --help | -h) usage; exit 0 ;;
    --baseline)
      [[ $# -ge 2 ]] || { echo "host-tmp-reading: --baseline needs a file" >&2; exit 2; }
      baseline=$2
      shift 2
      ;;
    --baseline=*) baseline=${1#--baseline=}; shift ;;
    -*) echo "host-tmp-reading: unexpected flag: $1" >&2; usage >&2; exit 2 ;;
    *) users+=("$1"); shift ;;
  esac
done

if [[ ${#users[@]} -eq 0 ]]; then
  echo "host-tmp-reading: name at least one user" >&2
  usage >&2
  exit 2
fi
if [[ -n $baseline && ! -r $baseline ]]; then
  echo "host-tmp-reading: cannot read baseline $baseline" >&2
  exit 2
fi

root="${TMPDIR_ROOT:-/tmp}"
read -r -a stat_cmd <<<"${STAT:-stat}"
prefixes=("runsheet-*" "latchkey-*" "mesh-home-*" "mesh-eval*" "mesh-session-*" "go-build*" "other")

# prefix_of <name>: the first of $prefixes the name matches, else other.
prefix_of() {
  local name=$1 p
  for p in "${prefixes[@]}"; do
    [[ $p == other ]] && break
    # shellcheck disable=SC2053 # $p is a glob, matched as one
    if [[ $name == $p ]]; then
      echo "$p"
      return
    fi
  done
  echo other
}

declare -A bytes=() entries=()
for u in "${users[@]}" all; do
  for p in "${prefixes[@]}"; do
    bytes["$u|$p"]=0
    entries["$u|$p"]=0
  done
done

notes=()
shopt -s nullglob dotglob
for path in "$root"/*; do
  [[ -L $path ]] && continue
  owner=$("${stat_cmd[@]}" -c %U -- "$path" 2>/dev/null) || { notes+=("cannot stat $path"); continue; }
  match=""
  for u in "${users[@]}"; do
    [[ $owner == "$u" ]] && { match=$u; break; }
  done
  [[ -n $match ]] || continue
  sz=$(du -sb -- "$path" 2>/dev/null | cut -f1) || true
  [[ -n $sz ]] || { notes+=("cannot size $path"); sz=0; }
  p=$(prefix_of "${path##*/}")
  bytes["$match|$p"]=$((bytes["$match|$p"] + sz))
  entries["$match|$p"]=$((entries["$match|$p"] + 1))
  bytes["all|$p"]=$((bytes["all|$p"] + sz))
  entries["all|$p"]=$((entries["all|$p"] + 1))
done
shopt -u nullglob dotglob

total_bytes=0
total_entries=0
echo "== /tmp by user and prefix"
for u in "${users[@]}" all; do
  for p in "${prefixes[@]}"; do
    echo -e "$u\t$p\tbytes=${bytes["$u|$p"]}\tentries=${entries["$u|$p"]}"
  done
done
for p in "${prefixes[@]}"; do
  total_bytes=$((total_bytes + bytes["all|$p"]))
  total_entries=$((total_entries + entries["all|$p"]))
done
echo -e "total\tbytes=$total_bytes\tentries=$total_entries"

if [[ ${#notes[@]} -gt 0 ]]; then
  echo
  for n in "${notes[@]}"; do
    echo "note: $n"
  done
fi

[[ -n $baseline ]] || exit 0

signed() {
  if [[ $1 -ge 0 ]]; then echo "+$1"; else echo "$1"; fi
}

declare -A base_bytes=() base_entries=()
base_total_bytes=0
base_total_entries=0
while IFS=$'\t' read -r f1 f2 f3 f4; do
  if [[ $f1 == total && $f2 == bytes=* && $f3 == entries=* ]]; then
    base_total_bytes=${f2#bytes=}
    base_total_entries=${f3#entries=}
  elif [[ $f3 == bytes=* && $f4 == entries=* ]]; then
    base_bytes["$f1|$f2"]=${f3#bytes=}
    base_entries["$f1|$f2"]=${f4#entries=}
  fi
done <"$baseline"

echo
echo "== since baseline"
changed=0
for u in "${users[@]}" all; do
  for p in "${prefixes[@]}"; do
    db=$((bytes["$u|$p"] - ${base_bytes["$u|$p"]:-0}))
    de=$((entries["$u|$p"] - ${base_entries["$u|$p"]:-0}))
    if [[ $db -ne 0 || $de -ne 0 ]]; then
      echo -e "$u\t$p\tbytes=$(signed "$db")\tentries=$(signed "$de")"
      changed=1
    fi
  done
done
[[ $changed -eq 1 ]] || echo "(no change)"
added_bytes=$((total_bytes - base_total_bytes))
added_entries=$((total_entries - base_total_entries))
echo -e "added\tbytes=$(signed "$added_bytes")\tentries=$(signed "$added_entries")"

echo
if [[ $added_bytes -eq 0 && $added_entries -eq 0 ]]; then
  echo "unchanged"
elif [[ $added_bytes -gt 0 || ($added_bytes -eq 0 && $added_entries -gt 0) ]]; then
  echo "grew"
else
  echo "shrank"
fi

exit 0
