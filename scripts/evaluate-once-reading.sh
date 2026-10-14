#!/usr/bin/env bash
# Takes the reading item 4 of an-items-evaluation-runs-on-one-node owes:
# across one or more node.log files, how many times each item was
# evaluated (`item <label>: evaluate on` lines — internal/node/evaluate.go's
# e.logf, one per evaluation session started) and how many `not yours`
# refusals the logs carry. The two are the goal's own evidence: an item
# two nodes both start evaluating logs `evaluate on` twice, once in
# each node's log, and the loser of the race has a claim, a progress
# report or an attestation refused `not yours` (ADR-015's
# NotYoursLine names the shape for registration; the gateway answers
# the same word for a claim, a progress report and an attestation that
# are no longer the caller's to send). The goal's acceptance — "after
# release, both dl360 node logs show one `evaluate on` per item and
# zero `not yours` lines over an hour" — is exactly these two counts,
# across dl360's two logs (/home/anythink/.mesh/node.log and
# /home/grapevine/.mesh/node.log), at zero.
#
# Like sandboxed-swap-reading.sh and capacity-changes.sh, this is a
# measurement (ADR-020's `Measure:` item): it exits 0 and prints what
# the logs show for a human to read, never a verdict a build is judged
# on.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: scripts/evaluate-once-reading.sh [--since TIME] [--until TIME] <node.log>...

Reads one or more node.log files (dl360's /home/anythink/.mesh/node.log
and /home/grapevine/.mesh/node.log, or any others) and prints, across
all of them combined:

  == evaluate on, by item
    one line per item label, in first-seen order across the logs:
      <label>: <n>
    an item seen more than once is flagged:
      <label>: <n>  MORE THAN ONCE

  == not yours
    not yours: <n>

--since and --until narrow to lines whose timestamp (node.log's own
`YYYY/MM/DD HH:MM:SS`, the same format either flag takes) is on or
after / on or before the given time; a line with no `mesh node: `
prefix is never counted, and a line outside the window counts toward
neither total.

The numbers this prints are not a gate (ADR-020): a reading, never a
build's verdict. Exits 0 always, even with no item evaluated twice and
no `not yours` line.
EOF
}

since=""
until_=""
logs=()
while [ $# -gt 0 ]; do
  case "$1" in
    --help | -h) usage; exit 0 ;;
    --since) since="$2"; shift 2 ;;
    --since=*) since="${1#--since=}"; shift ;;
    --until) until_="$2"; shift 2 ;;
    --until=*) until_="${1#--until=}"; shift ;;
    --) shift; while [ $# -gt 0 ]; do logs+=("$1"); shift; done ;;
    -*) echo "evaluate-once-reading: unknown argument: $1" >&2; usage >&2; exit 2 ;;
    *) logs+=("$1"); shift ;;
  esac
done

echo "== reading taken $(date '+%Y-%m-%d %H:%M:%S %z') on $(hostname)"

if [ ${#logs[@]} -eq 0 ]; then
  echo "no log given"
  exit 0
fi

present=()
for f in "${logs[@]}"; do
  if [ -f "$f" ]; then
    present+=("$f")
  else
    echo "log: $f not found"
  fi
done

if [ ${#present[@]} -eq 0 ]; then
  exit 0
fi

echo "logs: ${present[*]}"
[ -z "$since" ] || echo "since: $since"
[ -z "$until_" ] || echo "until: $until_"

# A single awk pass over every log given, in the order they were
# named: each `item <label>: evaluate on …` line counts once for its
# label (order on first sight, so the by-item section reads in the
# order items were first evaluated), and any line containing the words
# `not yours` counts once overall, whichever command or node logged it.
# Timestamps are node.log's own `YYYY/MM/DD HH:MM:SS`, fixed-width and
# zero-padded (as sandboxed-swap-reading.sh and capacity-changes.sh
# rely on), so a plain string compare against --since/--until orders
# and bounds them.
report="$(LC_ALL=C awk -v since="$since" -v until="$until_" '
{
  line = $0
  if (substr(line, 1, 11) != "mesh node: ") next
  ts = substr(line, 12, 19)
  rest = substr(line, 32)

  if (since != "" && ts < since) next
  if (until != "" && ts > until) next

  if (rest ~ /^item [^:]+: evaluate on /) {
    item = rest
    sub(/^item /, "", item)
    sub(/:.*/, "", item)
    if (!(item in count)) order[++n] = item
    count[item]++
    next
  }

  if (index(rest, "not yours") > 0) {
    notYours++
    next
  }
}
END {
  for (i = 1; i <= n; i++) printf "ITEM\t%s\t%d\n", order[i], count[order[i]]
  printf "NOTYOURS\t%d\n", notYours + 0
}
' "${present[@]}")"

echo
echo "== evaluate on, by item"
any=0
while IFS=$'\t' read -r kind a b; do
  [ "$kind" = "ITEM" ] || continue
  any=1
  if [ "$b" -gt 1 ]; then
    echo "$a: $b  MORE THAN ONCE"
  else
    echo "$a: $b"
  fi
done <<<"$report"
[ "$any" -eq 1 ] || echo "(none)"

echo
echo "== not yours"
notyours=$(awk -F'\t' '$1=="NOTYOURS"{print $2}' <<<"$report")
echo "not yours: ${notyours:-0}"

exit 0
