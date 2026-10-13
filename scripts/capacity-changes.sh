#!/usr/bin/env bash
# Takes the reading item 4 of the-node-backs-off-a-hot-host: per UTC day,
# the count of `capacity N → M` lines and how many were driven by `host
# hot`, `host quiet` and other reasons (ADR-020: a measurement is
# informational, never a merge gate).
set -euo pipefail

usage() {
  cat <<'EOF'
usage: scripts/capacity-changes.sh [logfile] [--help]

Reads a node.log file (default $MESH_HOME/node.log, or a path argument)
and prints, per UTC day, the count of `capacity N → M` lines and how many
were `host hot`, `host quiet` and other reasons, one line per day:

  YYYY-MM-DD <total> (<host hot>, <host quiet>, <other>)

The counts are of the capacity change lines found in the log for that day.
The categories are mutually exclusive: each line is counted in exactly one.
EOF
}

logfile="${1:-.}"
if [ "$logfile" = "--help" ] || [ "$logfile" = "-h" ]; then
  usage
  exit 0
fi

if [ "$logfile" = "." ]; then
  # Default: $MESH_HOME/node.log
  logfile="${MESH_HOME:-$HOME/.mesh}/node.log"
fi

if [ ! -f "$logfile" ]; then
  echo "capacity-changes: logfile not found: $logfile" >&2
  exit 1
fi

# Regex to match capacity change lines:
# Lines like:
#   capacity 8 → 7: host hot: load 54/18 cores
#   capacity 2 → 3: host quiet 5m
#   capacity 1 → 2: clamped into 1–3
#   capacity N → M: <reason> (...)
# The timestamp is ISO8601 at the start: YYYY-MM-DDTHH:MM:SS...Z

# Parse and categorize capacity changes by day
# Use awk to parse, then sort with sort command for portability
awk '
  /capacity [0-9]+ → [0-9]+:/ {
    # Extract date from ISO timestamp (YYYY-MM-DDTHH:MM:SSZ)
    # Timestamp is typically at the start or after a prefix
    match($0, /[0-9]{4}-[0-9]{2}-[0-9]{2}/)
    if (RSTART > 0) {
      date = substr($0, RSTART, 10)
    } else {
      next
    }

    # Categorize the reason
    if ($0 ~ /host hot/) {
      reason = "host hot"
    } else if ($0 ~ /host quiet/) {
      reason = "host quiet"
    } else {
      reason = "other"
    }

    days[date]++
    reasons[date, reason]++
  }
  END {
    # Output day totals with categorized counts
    for (day in days) {
      total = days[day]
      host_hot = reasons[day, "host hot"] + 0
      host_quiet = reasons[day, "host quiet"] + 0
      other = reasons[day, "other"] + 0
      printf "%s %d (host hot %d, host quiet %d, other %d)\n", day, total, host_hot, host_quiet, other
    }
  }
' "$logfile" | sort
