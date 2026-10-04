#!/usr/bin/env bash
# Takes the reading goal the-node-backs-off-a-hot-host owes (item 5,
# ADR-020: a measurement of the host is informational, never a merge
# gate): the load, cores and package temperature the back-off (ADR-023)
# judges, and the node's capacity line, so the operator can watch
# capacity fall under load and climb back once the host is quiet.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: scripts/host-backoff-reading.sh [--interval S] [--count N] [--help]

Prints one dated reading (repeated every --interval seconds, --count
times; default: once):

  load         the 1/5/15-minute load average, from /proc/loadavg
  cores        `nproc`
  temperature  the x86_pkg_temp or TCPU thermal zone's reading in °C,
               else `no temperature zone`
  node         `mesh node status`'s own output, when a node looks to be
               running here (~/.mesh/node.pid, or the mesh binary is
               found), else `no node running`
  host         the `host:` thresholds in $MESH_HOME/node.yaml, when it
               has one

$MESH_HOME (default ~/.mesh) is the node's home; $MESH_BIN (default
`mesh`) is the binary `mesh node status` is run with; $STATUS_TIMEOUT
(default 60) is how long it is given.
EOF
}

interval=0
count=1
count_set=0
while [ $# -gt 0 ]; do
  case "$1" in
    --help | -h) usage; exit 0 ;;
    --interval) interval="$2"; shift 2 ;;
    --interval=*) interval="${1#--interval=}"; shift ;;
    --count) count="$2"; count_set=1; shift 2 ;;
    --count=*) count="${1#--count=}"; count_set=1; shift ;;
    *) echo "host-backoff-reading: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done
case "$interval" in '' | *[!0-9]*) echo "host-backoff-reading: --interval wants a whole number of seconds, got '$interval'" >&2; exit 2 ;; esac
case "$count" in '' | *[!0-9]*) echo "host-backoff-reading: --count wants a whole number, got '$count'" >&2; exit 2 ;; esac
if [ "$interval" -gt 0 ] && [ "$count_set" = 0 ]; then
  count=0 # repeat forever until killed
fi

home="${MESH_HOME:-$HOME/.mesh}"
mesh_bin="${MESH_BIN:-mesh}"

# package_temp reads the x86_pkg_temp or TCPU thermal zone's millidegree
# reading and prints it in °C; nothing printed when there is no such
# zone (no /sys/class/thermal, or neither type present) — not an error,
# the same reading internal/node/machine.go's ProcMachine takes.
package_temp() {
  [ -d /sys/class/thermal ] || return 0
  for zone in /sys/class/thermal/thermal_zone*; do
    [ -d "$zone" ] || continue
    case "$(cat "$zone/type" 2>/dev/null || true)" in
      x86_pkg_temp | TCPU)
        raw="$(cat "$zone/temp" 2>/dev/null || true)"
        [ -n "$raw" ] || continue
        awk -v m="$raw" 'BEGIN { printf "%.1f", m / 1000 }'
        return 0
        ;;
    esac
  done
  return 0
}

# host_thresholds prints the `host:` block of node.yaml ($home/node.yaml)
# verbatim (the line and whatever follows it indented), when there is
# one.
host_thresholds() {
  [ -f "$home/node.yaml" ] || return 0
  awk '
    /^host:/ { found=1; print; next }
    found && /^[[:space:]]/ { print; next }
    found { exit }
  ' "$home/node.yaml"
}

take_reading() {
  echo "== host-backoff-reading taken $(date -u '+%Y-%m-%dT%H:%M:%SZ') on $(hostname)"

  if [ -r /proc/loadavg ]; then
    read -r one five fifteen _ </proc/loadavg
    echo "load: 1min=$one 5min=$five 15min=$fifteen"
  else
    echo "load: /proc/loadavg not readable"
  fi

  if command -v nproc >/dev/null 2>&1; then
    echo "cores: $(nproc)"
  else
    echo "cores: nproc not found"
  fi

  temp="$(package_temp)"
  if [ -n "$temp" ]; then
    echo "temperature: ${temp}°C"
  else
    echo "temperature: no temperature zone"
  fi

  if [ -f "$home/node.pid" ] || command -v "$mesh_bin" >/dev/null 2>&1; then
    if command -v "$mesh_bin" >/dev/null 2>&1; then
      echo "node:"
      if out="$(timeout "${STATUS_TIMEOUT:-60}" "$mesh_bin" node status 2>&1)"; then
        printf '%s\n' "$out"
      else
        rc=$?
        printf '%s\n' "$out"
        echo "($mesh_bin node status exited $rc; 124 is the ${STATUS_TIMEOUT:-60}s timeout)"
      fi
    else
      echo "node: no node running ($home/node.pid is present but $mesh_bin was not found)"
    fi
  else
    echo "node: no node running"
  fi

  thresholds="$(host_thresholds)"
  if [ -n "$thresholds" ]; then
    echo "host:"
    printf '%s\n' "$thresholds"
  else
    echo "host: no host: section in $home/node.yaml"
  fi
}

n=0
while :; do
  take_reading
  n=$((n + 1))
  if [ "$count" -gt 0 ] && [ "$n" -ge "$count" ]; then
    break
  fi
  [ "$interval" -gt 0 ] || break
  sleep "$interval"
done
