#!/usr/bin/env bash
# Takes the reading goal node-yaml-takes-effect-or-says-why-not owes: what
# the node runs, what node.yaml says, the capacity reloads and reload
# errors node.log recorded, how fast node.log grows, how the service
# writes it, and what the platform reports. Run it on a node.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: scripts/node-reload-reading.sh [--hours N] [--now "YYYY-MM-DD HH:MM:SS"] [--help]

Prints, each section labelled, for the node whose home is $MESH_HOME
(default ~/.mesh):

  version      `mesh version` and the latest `mesh node v… pid … config …`
               line of node.log
  node.yaml    its mtime (stat -c %y) and its `capacity:` line
  reloads      every `capacity … → …` and `reload config:` line of node.log
               and node.log.1 … node.log.5 in the last N hours (--hours,
               default 24), found by each line's timestamp; a rotated file
               last written before the window is not read at all
  logs         ls -l of node.log* and sessions.log*, and node.log's bytes
               per hour since it began (node.log.1's mtime, the rotation)
  unit         systemctl --user cat io.projectmesh.mesh-node, its Standard*
               and ExecStart lines, when systemd is present
  status       `mesh node status`, when $MESH_HOME/token exists

--now sets the clock the window is measured from (default: now).

The mesh binary is the one the node runs, never whichever `mesh` is first
on PATH: $MESH_BIN when set; else the running node's own executable,
/proc/<pid>/exe of the pid in $MESH_HOME/node.pid (a binary swapped since
the start still runs from there); else the first word of the service
unit's ExecStart; else `mesh` on PATH. The section says which. `mesh node
status` is given $STATUS_TIMEOUT seconds (default 60).
EOF
}

hours=24
now=""
while [ $# -gt 0 ]; do
  case "$1" in
    --help | -h) usage; exit 0 ;;
    --hours) hours="$2"; shift 2 ;;
    --hours=*) hours="${1#--hours=}"; shift ;;
    --now) now="$2"; shift 2 ;;
    --now=*) now="${1#--now=}"; shift ;;
    *) echo "node-reload-reading: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done
case "$hours" in '' | *[!0-9]*) echo "node-reload-reading: --hours wants a whole number, got '$hours'" >&2; exit 2 ;; esac

home="${MESH_HOME:-$HOME/.mesh}"
proc="${MESH_PROC:-/proc}"
log="$home/node.log"
now_s="$(if [ -n "$now" ]; then date -d "$now" +%s; else date +%s; fi)"
since_s=$((now_s - hours * 3600))
# node.log's lines open `mesh node: YYYY/MM/DD HH:MM:SS`, in the node's local time.
since="$(date -d "@$since_s" '+%Y/%m/%d %H:%M:%S')"

echo "== reading taken $(date -d "@$now_s" '+%Y-%m-%d %H:%M:%S %z') on $(hostname), MESH_HOME=$home"

# The binary the node runs, and how it was found.
mesh=""
how=""
pid="$(cat "$home/node.pid" 2>/dev/null || true)"
if [ -n "${MESH_BIN:-}" ]; then
  mesh="$MESH_BIN"
  how="MESH_BIN"
elif [ -n "$pid" ] && [ -x "$proc/$pid/exe" ]; then
  mesh="$proc/$pid/exe"
  how="the running node, pid $pid in node.pid, -> $(readlink "$proc/$pid/exe" 2>/dev/null || echo '?')"
elif command -v systemctl >/dev/null 2>&1 \
  && exec_start="$(systemctl --user show -p ExecStart --value io.projectmesh.mesh-node 2>/dev/null)" \
  && unit_bin="$(sed -n 's/.*path=\([^ ;]*\).*/\1/p' <<<"$exec_start")" && [ -x "$unit_bin" ]; then
  mesh="$unit_bin"
  how="the unit's ExecStart (no live pid in node.pid)"
else
  mesh="mesh"
  how="PATH: $(command -v mesh || echo 'not found') (no live pid in node.pid, no unit)"
fi

echo "== version"
echo "binary: $mesh ($how)"
if [ -x "$mesh" ] || command -v "$mesh" >/dev/null 2>&1; then
  echo "mesh version: $("$mesh" version 2>&1)"
else
  echo "mesh version: $mesh not found"
fi
if [ -f "$log" ]; then
  start="$(grep -a -E '^mesh node: [0-9/]+ [0-9:]+ mesh node v[^ ]*, pid [0-9]+, config ' "$log" | tail -1 || true)"
  echo "in force: ${start:-no mesh node v… pid line in node.log}"
else
  echo "in force: no $log"
fi

echo "== node.yaml"
if [ -f "$home/node.yaml" ]; then
  echo "mtime: $(stat -c '%y' "$home/node.yaml")"
  cap="$(grep -n -E '^[[:space:]]*capacity:' "$home/node.yaml" || true)"
  echo "capacity: ${cap:-no capacity: line}"
else
  echo "no $home/node.yaml"
fi

echo "== reloads since $since (last $hours h)"
found=0
for f in "$log".5 "$log".4 "$log".3 "$log".2 "$log".1 "$log"; do
  [ -f "$f" ] || continue
  if [ "$(stat -c %Y "$f")" -lt "$since_s" ]; then
    echo "($(basename "$f") last written $(stat -c '%y' "$f"), before the window: not read)"
    continue
  fi
  lines="$(LC_ALL=C awk -v since="$since" -v name="$(basename "$f")" '
    substr($0, 1, 11) == "mesh node: " {
      ts = substr($0, 12, 19)
      if (ts >= since && ($0 ~ /^mesh node: [0-9\/]+ [0-9:]+ capacity [^ ]+ → / || index($0, " reload config: ") > 0))
        print name ": " $0
    }' "$f")"
  if [ -n "$lines" ]; then
    printf '%s\n' "$lines"
    found=1
  fi
done
[ "$found" = 1 ] || echo "none"

echo "== logs"
# shellcheck disable=SC2012
ls -l --time-style=full-iso "$home"/node.log* "$home"/sessions.log* 2>&1 || true
if [ -f "$log" ] && [ -f "$log.1" ]; then
  size="$(stat -c %s "$log")"
  began="$(stat -c %Y "$log.1")"
  last="$(stat -c %Y "$log")"
  secs=$((last - began))
  if [ "$secs" -gt 0 ]; then
    echo "node.log: $size bytes in $secs s since node.log.1 ($(date -d "@$began" '+%Y-%m-%d %H:%M:%S')), $((size * 3600 / secs)) bytes/hour"
  else
    echo "node.log: $size bytes, written no later than node.log.1: bytes/hour unknown"
  fi
else
  echo "node.log: no node.log.1 to date its start: bytes/hour unknown"
fi

echo "== unit"
if command -v systemctl >/dev/null 2>&1 && systemctl --user cat io.projectmesh.mesh-node >/dev/null 2>&1; then
  systemctl --user cat io.projectmesh.mesh-node | grep -E 'Standard|ExecStart' || echo "no Standard or ExecStart line"
else
  echo "no systemd unit io.projectmesh.mesh-node"
fi

echo "== status"
if [ -f "$home/token" ] && { [ -x "$mesh" ] || command -v "$mesh" >/dev/null 2>&1; }; then
  timeout "${STATUS_TIMEOUT:-60}" "$mesh" node status 2>&1 || echo "(mesh node status exited $?; 124 is the ${STATUS_TIMEOUT:-60} s timeout)"
else
  echo "no $home/token or no $mesh: mesh node status not run"
fi
