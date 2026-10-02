#!/usr/bin/env bash
# Takes the reading goal one-evaluator-for-ordinary-work owes: the
# `evaluate <priority>:` rows of `mesh node usage` for a window, on
# their own, so the mini's before (two evaluators for everything) and
# after (one evaluator for normal and low) readings are one command
# each.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: scripts/evaluation-cost-by-priority.sh (--since YYYY-MM-DD | --from YYYY-MM-DD --to YYYY-MM-DD) [--log PATH] [--help]

Runs `mesh node usage --from <from> --to <to> [--log <log>] --kind
evaluate` and prints only its evaluation cost by priority, one row per
priority present (urgent, high, normal, low, then unknown for records
logged before adr-016's amendment 2026-10-02):

  evaluate <priority>: <n> items, <a>×1 / <b>×2, median $<x>/item, total $<y>

--since is the window from that day through today (--to is tomorrow).
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
    --since) from=$2; to=$(date -u -d tomorrow +%F 2>/dev/null || date -u -v+1d +%F); shift 2 ;;
    --from) from=$2; shift 2 ;;
    --to) to=$2; shift 2 ;;
    --log) log=$2; shift 2 ;;
    --help | -h) usage; exit 0 ;;
    *) echo "evaluation-cost-by-priority: unknown argument '$1'" >&2; usage >&2; exit 2 ;;
  esac
done

if [ -z "$from" ] || [ -z "$to" ]; then
  echo "evaluation-cost-by-priority: --since, or --from and --to, are required" >&2
  usage >&2
  exit 2
fi

mesh=${MESH_BIN:-mesh}

args=(node usage --from "$from" --to "$to" --kind evaluate)
if [ -n "$log" ]; then
  args+=(--log "$log")
fi

out=$("$mesh" "${args[@]}")

rows=$(grep -E '^evaluate [^ ]+: ' <<<"$out" || true)
if [ -z "$rows" ]; then
  echo "evaluation-cost-by-priority: no evaluate records in the window" >&2
  exit 1
fi
printf '%s\n' "$rows"
