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
and prints, per day (local time from the log), the count of `capacity N → M`
lines and how many were `host hot`, `host quiet` and other reasons,
one line per day:

  YYYY-MM-DD <total> (host hot <count>, host quiet <count>, other <count>)

The counts are of the capacity change lines found in the log for that day.
The categories are mutually exclusive: each line is counted in exactly one.

The log format is expected to be: mesh node: YYYY/MM/DD HH:MM:SS message
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

# Parse and categorize capacity changes by day
# The log format is: mesh node: YYYY/MM/DD HH:MM:SS message
# We extract the date (YYYY/MM/DD) and convert it to YYYY-MM-DD
awk '
  /capacity [0-9]+ → [0-9]+/ {
    # Extract date from "mesh node: YYYY/MM/DD HH:MM:SS ..."
    # Find the position of "mesh node: " and extract YYYY/MM/DD
    idx = index($0, "mesh node: ")
    if (idx > 0) {
      # Start after "mesh node: "
      rest = substr($0, idx + 11)
      # Extract YYYY/MM/DD (10 characters)
      datestr = substr(rest, 1, 10)
      # Convert YYYY/MM/DD to YYYY-MM-DD
      if (datestr ~ /^[0-9]{4}\/[0-9]{2}\/[0-9]{2}$/) {
        date = substr(datestr, 1, 4) "-" substr(datestr, 6, 2) "-" substr(datestr, 9, 2)
      } else {
        next
      }
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
