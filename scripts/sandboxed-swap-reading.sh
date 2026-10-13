#!/usr/bin/env bash
# Takes the reading item 5 of a-sandboxed-session-outlives-the-node
# owes: for each self-update swap a node.log records, which build
# sessions were running when it swapped, and whether the process it
# restarted into logged adopting each one and finishing its item — so
# an upgrade that silently drops a sandboxed session (adopted: no, or
# adopted but never finished) is visible without reading the whole log
# by hand.
#
# A swap is `internal/node/selfupdate.go`'s Loop.update: the line just
# before it, `self-update: <to> verified; no drain: swapping and
# restarting, sessions left running for the new binary to adopt`,
# carries ` (sandbox: on)` when the node ran its sessions in pods
# (l.sandboxNote); the swap itself is `self-update: <to> is in place;
# exiting for the service to start it, <n> session(s) left for it to
# adopt`. Only a sandboxed swap's sessions are reported — a host
# session already outlives the node through its own systemd/launchd
# unit (adr-002), the thing this goal and this reading are about is the
# sandboxed path (adr-007's and adr-009's amendments 2026-10-13).
#
# A session is "running at" a swap when its build-start line
# (`item <label>: build on …`) precedes the swap with no finish before
# it. The process the swap restarts into "adopts" it by logging
# `item <label>: adopted its running session (container …, from byte
# …)` (internal/node/runner.go's AdoptItem); it "finishes" the item by
# logging `item <label>: pushed <sha>; finished <branch> ($<cost>)` or
# `item <label>: finished already done ($<cost>): <evidence>`
# (internal/node/runner.go's afterBuild). Seconds to the first adopted
# heartbeat is the time from the swap line to that adopted line — the
# window ADR-009's "the item heartbeated at once so the stall window
# never elapses" owes.
#
# Like host-backoff-reading.sh and mcp-auth-reading.sh, this is a
# measurement (ADR-020's `Measure:` item): it exits 0 and prints what
# the log shows for a human to read, never a verdict a build is judged
# on. A log with no sandboxed swap in it prints none, not a failure.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: scripts/sandboxed-swap-reading.sh [--log PATH | PATH] [--help]

PATH (or --log PATH) is the node.log to read; default
$MESH_HOME/node.log ($MESH_HOME default ~/.mesh — ADR-019,
internal/node/logfile.go's ServiceLog).

Prints:

  == self-update swaps
    one line per self-update swap found, in order:
      swap <n>: <version> at <timestamp> (sandboxed, <k> declared)
      swap <n>: <version> at <timestamp> (not sandboxed; skipped)

  == sessions
    for every sandboxed swap, one line per build session that was
    running at it (a build-start line before the swap with no finish
    before it):
      swap <n> <version>: item <label>: adopted <yes|no> (<secs>s), finished <yes|no>
    <secs> is the time from the swap to the first line logging that
    item adopted; "-" when it was never adopted.

The numbers this prints are not a gate (ADR-020): a reading, never a
build's verdict.
EOF
}

log=""
while [ $# -gt 0 ]; do
  case "$1" in
    --help | -h) usage; exit 0 ;;
    --log) log="$2"; shift 2 ;;
    --log=*) log="${1#--log=}"; shift ;;
    --) shift; [ $# -gt 0 ] && { log="$1"; shift; }; ;;
    -*) echo "sandboxed-swap-reading: unknown argument: $1" >&2; usage >&2; exit 2 ;;
    *) log="$1"; shift ;;
  esac
done

home="${MESH_HOME:-$HOME/.mesh}"
log="${log:-$home/node.log}"

echo "== reading taken $(date '+%Y-%m-%d %H:%M:%S %z') on $(hostname)"
if [ ! -f "$log" ]; then
  echo "log: $log not found"
  exit 0
fi
echo "log: $log"

# A single awk pass over the file, in two parts of one END block: the
# main loop classifies each `mesh node: ` line and builds, per item,
# its chronological list of adopted and finished timestamps (joined by
# "::", which no timestamp or item label contains), and per swap the
# items that were still open (build-started, not yet finished) when it
# happened; the END block then prints each swap and, for a sandboxed
# one, each of its open items' first adopted/finished timestamp after
# it. Timestamps are node.log's own `YYYY/MM/DD HH:MM:SS`, fixed-width
# and zero-padded, so a plain string compare orders them (as
# node-reload-reading.sh and mcp-auth-reading.sh rely on) and a plain
# string `>` finds the first one after a swap.
report="$(LC_ALL=C awk '
{
  line = $0
  if (substr(line, 1, 11) != "mesh node: ") next
  ts = substr(line, 12, 19)
  rest = substr(line, 32)

  if (rest ~ /^self-update: [^ ]+ verified; no drain:/) {
    pendingSandboxed = (rest ~ /\(sandbox: on\)$/) ? 1 : 0
    next
  }

  if (rest ~ /^self-update: [^ ]+ is in place; exiting for the service to start it, [0-9]+ session\(s\) left for it to adopt$/) {
    swapCount++
    split(rest, sparts, " ")
    swapVersion[swapCount] = sparts[2]
    swapTs[swapCount] = ts
    swapSandboxed[swapCount] = (pendingSandboxed == 1) ? 1 : 0
    declared = rest
    sub(/.*it, /, "", declared)
    sub(/ session\(s\).*/, "", declared)
    swapDeclared[swapCount] = declared
    pendingSandboxed = 0
    openJoined = ""
    for (k = 1; k <= itemOrderCount; k++) {
      it = itemOrder[k]
      if (itemState[it] == "open") openJoined = openJoined it "::"
    }
    swapOpen[swapCount] = openJoined
    next
  }

  if (rest ~ /^item [^:]+: build on /) {
    item = rest
    sub(/^item /, "", item)
    sub(/:.*/, "", item)
    if (!(item in itemState)) {
      itemOrderCount++
      itemOrder[itemOrderCount] = item
    }
    itemState[item] = "open"
    next
  }

  if (rest ~ /^item [^:]+: adopted its (running|ended) session \(/) {
    item = rest
    sub(/^item /, "", item)
    sub(/:.*/, "", item)
    adoptedList[item] = adoptedList[item] ts "::"
    next
  }

  if (rest ~ /^item [^:]+: (pushed .*; finished |finished already done )/) {
    item = rest
    sub(/^item /, "", item)
    sub(/:.*/, "", item)
    itemState[item] = "closed"
    finishedList[item] = finishedList[item] ts "::"
    next
  }
}
END {
  for (k = 1; k <= swapCount; k++) {
    if (swapSandboxed[k] == 1) {
      printf "SWAP\t%d\t%s\t%s\tsandboxed, %s declared\n", k, swapVersion[k], swapTs[k], swapDeclared[k]
    } else {
      printf "SWAP\t%d\t%s\t%s\tnot sandboxed; skipped\n", k, swapVersion[k], swapTs[k]
      continue
    }
    n = split(swapOpen[k], items, "::")
    for (i = 1; i <= n; i++) {
      it = items[i]
      if (it == "") continue
      adoptedTs = ""
      m = split(adoptedList[it], al, "::")
      for (j = 1; j <= m; j++) {
        if (al[j] != "" && al[j] > swapTs[k]) { adoptedTs = al[j]; break }
      }
      finishedTs = ""
      m2 = split(finishedList[it], fl, "::")
      for (j = 1; j <= m2; j++) {
        if (fl[j] != "" && fl[j] > swapTs[k]) { finishedTs = fl[j]; break }
      }
      adoptedYN = (adoptedTs != "") ? "yes" : "no"
      finishedYN = (finishedTs != "") ? "yes" : "no"
      # never the empty string: two adjacent tabs collapse under the
      # tab-delimited read below, in bash, and shift every field after
      if (adoptedTs == "") adoptedTs = "-"
      printf "SESSION\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n", k, swapVersion[k], swapTs[k], it, adoptedYN, adoptedTs, finishedYN
    }
  }
}
' "$log")"

echo
echo "== self-update swaps"
while IFS=$'\t' read -r kind a b c d; do
  [ "$kind" = "SWAP" ] || continue
  # a: index, b: version, c: ts, d: note (sandboxed, N declared | not sandboxed; skipped)
  echo "swap $a: $b at $c ($d)"
done <<<"$report"

echo
echo "== sessions"
while IFS=$'\t' read -r kind a b c d e f g; do
  [ "$kind" = "SESSION" ] || continue
  # a: index, b: version, c: swap ts, d: item, e: adopted y/n,
  # f: adopted ts (empty when e is no), g: finished y/n
  if [ "$e" = "yes" ]; then
    secs=$(( $(date -d "$f" +%s) - $(date -d "$c" +%s) ))
    echo "swap $a $b: item $d: adopted yes (${secs}s), finished $g"
  else
    echo "swap $a $b: item $d: adopted no (-), finished $g"
  fi
done <<<"$report"

exit 0
