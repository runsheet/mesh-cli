#!/usr/bin/env bash
# Takes the reading item 4 of a-sessions-mcp-token-outlives-the-session
# owes: how many lines of node.log, in a window, say the gateway's /mcp
# rejected the node's token (`mesh-context: first call errored: MCP
# server "mesh-context" rejected the Authorization header in its
# config (update it, then run /mcp to reconnect)`) — Chris's watcher,
# 2026-10-11 05:20Z, found ten of them across both DL360 nodes in a
# few hours, matching the gateway's /mcp answering ~14 POST 401s per
# 40 minutes in Cloud Logging. The node writes the MCP entry with its
# own rotating machine-credential token (internal/node/workspace.go
# PrologueMCP); a session that outlives it loses every tool.
#
# Like mesh-home-reading.sh and host-backoff-reading.sh, this is a
# measurement (ADR-020's `Measure:` item): it exits 0 and prints a
# count for a human to read, never a verdict a build is judged on. No
# gcloud on PATH, or no --project, is a reading too — the Cloud
# Logging half is printed as skipped, not a failure.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: scripts/mcp-auth-reading.sh [--log PATH] [--hours N] [--now "YYYY-MM-DD HH:MM:SS"] [--project ID] [--help]

Prints, for the window ending --now (default: now) and running back
--hours hours (default 24):

  == node.log
    the count of --log's (default $MESH_HOME/node.log,
    $MESH_HOME default ~/.mesh — internal/node/logfile.go's
    ServiceLog) lines in the window whose timestamp
    (node.log's own `mesh node: YYYY/MM/DD HH:MM:SS`) falls in it and
    which contain `rejected the Authorization header`:
      rejected Authorization header lines: <count>

  == /mcp 401s (Cloud Logging)
    with gcloud on PATH and --project given, the count of POST 401
    responses on /mcp in the same window, by the gcloud logging read
    filter this script runs (below):
      /mcp POST 401s: <count>
    else one line saying which half is missing and that this section
    is skipped:
      skipped: no gcloud on PATH
      skipped: no --project given

The numbers this prints are not a gate (ADR-020): a reading, never a
build's verdict.

$MESH_HOME (default ~/.mesh) is the node's home, for the default log
path; $GCLOUD (default `gcloud`) is the binary run for the Cloud
Logging half.
EOF
}

hours=24
now=""
log=""
project=""
while [ $# -gt 0 ]; do
  case "$1" in
    --help | -h) usage; exit 0 ;;
    --log) log="$2"; shift 2 ;;
    --log=*) log="${1#--log=}"; shift ;;
    --hours) hours="$2"; shift 2 ;;
    --hours=*) hours="${1#--hours=}"; shift ;;
    --now) now="$2"; shift 2 ;;
    --now=*) now="${1#--now=}"; shift ;;
    --project) project="$2"; shift 2 ;;
    --project=*) project="${1#--project=}"; shift ;;
    *) echo "mcp-auth-reading: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done
case "$hours" in '' | *[!0-9]*) echo "mcp-auth-reading: --hours wants a whole number, got '$hours'" >&2; exit 2 ;; esac

home="${MESH_HOME:-$HOME/.mesh}"
log="${log:-$home/node.log}"
gcloud_bin="${GCLOUD:-gcloud}"

now_s="$(if [ -n "$now" ]; then date -d "$now" +%s; else date +%s; fi)"
since_s=$((now_s - hours * 3600))
# node.log's lines open `mesh node: YYYY/MM/DD HH:MM:SS`, in the node's
# local time; the format is fixed-width and zero-padded, so a plain
# string compare orders the same as the time it names (node-reload-
# reading.sh's same trick).
since="$(date -d "@$since_s" '+%Y/%m/%d %H:%M:%S')"
to="$(date -d "@$now_s" '+%Y/%m/%d %H:%M:%S')"
since_iso="$(date -u -d "@$since_s" '+%Y-%m-%dT%H:%M:%SZ')"
to_iso="$(date -u -d "@$now_s" '+%Y-%m-%dT%H:%M:%SZ')"

echo "== reading taken $(date -d "@$now_s" '+%Y-%m-%d %H:%M:%S %z') on $(hostname)"
echo "window: last ${hours}h, $since to $to"

echo
echo "== node.log"
if [ ! -f "$log" ]; then
  echo "log: $log not found"
  rejected_count=0
else
  echo "log: $log"
  rejected_count="$(LC_ALL=C awk -v since="$since" -v to="$to" '
    substr($0, 1, 11) == "mesh node: " {
      ts = substr($0, 12, 19)
      if (ts >= since && ts < to && index($0, "rejected the Authorization header") > 0) c++
    }
    END { print c + 0 }
  ' "$log")"
fi
echo "rejected Authorization header lines: $rejected_count"

echo
echo "== /mcp 401s (Cloud Logging)"
filter="httpRequest.requestMethod=\"POST\" AND httpRequest.requestUrl:\"/mcp\" AND httpRequest.status=401 AND timestamp>=\"$since_iso\" AND timestamp<\"$to_iso\""
echo "filter: $filter"
if ! command -v "$gcloud_bin" >/dev/null 2>&1; then
  echo "skipped: no gcloud on PATH"
elif [ -z "$project" ]; then
  echo "skipped: no --project given"
else
  if mcp_401_lines="$("$gcloud_bin" logging read "$filter" --project="$project" --format='value(timestamp)' 2>&1)"; then
    mcp_401_count=0
    [ -z "$mcp_401_lines" ] || mcp_401_count="$(printf '%s\n' "$mcp_401_lines" | grep -c .)"
    echo "/mcp POST 401s: $mcp_401_count"
  else
    echo "skipped: $gcloud_bin logging read failed: $mcp_401_lines"
  fi
fi

exit 0
