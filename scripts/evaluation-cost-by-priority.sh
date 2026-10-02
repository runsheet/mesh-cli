#!/usr/bin/env bash
# Takes the reading goal one-evaluator-for-ordinary-work owes: the
# `evaluation cost per item by priority` section of `mesh node usage`
# for a window, on its own, so the mini's before (two evaluators for
# everything) and after (one evaluator for normal and low) readings
# are one command each.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: scripts/evaluation-cost-by-priority.sh --from YYYY-MM-DD --to YYYY-MM-DD [--log PATH] [--help]

Runs `mesh node usage --from <from> --to <to> [--log <log>] --kind
evaluate` and prints only the `evaluation cost per item by priority`
section: one row per priority (urgent, high, normal, low, unknown for
records logged before adr-016's amendment 2026-10-02) with items,
evaluations, cost total, cost per item (median and mean), and the
share evaluated twice.

--log defaults to ~/.mesh/node.log and its rotated files, as `mesh node
usage` itself does. The mesh binary is $MESH_BIN when set, else `mesh`
on PATH.
EOF
}

from=""
to=""
log=""

while [ $# -gt 0 ]; do
  case "$1" in
    --from) from=$2; shift 2 ;;
    --to) to=$2; shift 2 ;;
    --log) log=$2; shift 2 ;;
    --help | -h) usage; exit 0 ;;
    *) echo "evaluation-cost-by-priority: unknown argument '$1'" >&2; usage >&2; exit 2 ;;
  esac
done

if [ -z "$from" ] || [ -z "$to" ]; then
  echo "evaluation-cost-by-priority: --from and --to are required" >&2
  usage >&2
  exit 2
fi

mesh=${MESH_BIN:-mesh}

args=(node usage --from "$from" --to "$to" --kind evaluate)
if [ -n "$log" ]; then
  args+=(--log "$log")
fi

out=$("$mesh" "${args[@]}")

awk '
  /^evaluation cost per item by priority$/ { found = 1 }
  found && /^unparsed [0-9]+$/ { exit }
  found { print }
' <<<"$out"

if ! grep -q '^evaluation cost per item by priority$' <<<"$out"; then
  echo "evaluation-cost-by-priority: no evaluate records in the window" >&2
  exit 1
fi
