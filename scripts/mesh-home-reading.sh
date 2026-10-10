#!/usr/bin/env bash
# Takes the reading item 4 of a-sessions-temporary-home-is-removed-when-its-pod-ends
# owes: how many of this user's ${TMPDIR:-/tmp}/mesh-home-* directories
# a running container still mounts, and how many podman pods and
# containers are left with nothing running — the dl360's 2026-10-10
# incident (370 dirs, 150 GB, in a day; 14 in use; 27/21 exited
# containers and 11/8 pods with nothing running on the two node users)
# is the goal's statement this reads against. Until the sweep (item 3)
# ships, the hourly cron on the dl360 runs this shape by hand; this
# script is that reading, not the sweep itself.
#
# Like host-backoff-reading.sh and withdrawn-account-reading.sh, this
# is a measurement (ADR-020's `Measure:` item): it exits 0 and prints
# `ok` or `stale` for a human to read, never a verdict a build is
# judged on. podman missing, or refusing every call, is a reading too
# — printed as its own line, not a failure.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: scripts/mesh-home-reading.sh [--help]

For the user it runs as, reads four sections and a verdict.

  == mesh-home dirs
    one line per $MESH_HOME_ROOT/mesh-home-* directory:
      <path>	bytes=<n>	mounted=<yes|no>
    mounted is yes when a running container's mounts
    (`podman ps --format json`, the `Mounts` field) name the
    directory's own path — the way the sandbox mounts a session's HOME
    at the same path inside its pod (internal/sandbox/sandbox.go's
    Session.Mounts). Followed by:
      dirs: <count>	bytes: <total>	mounted: <mounted>/<count>

  == running mesh pods
    `podman pod ps --format json` entries named mesh-* whose Status is
    Running, one line each:
      <name>	status=<status>

  == pods not running
    the same listing's entries named mesh-* whose Status is not
    Running, one line each:
      <name>	status=<status>

  == exited mesh containers
    `podman ps -a --filter status=exited --format json` entries named
    mesh-*, one line each:
      <name>	pod=<pod>	status=<status>

and then the verdict:

  ok

when every mesh-home dir is mounted and every mesh pod is Running,
else

  stale
  <path>	reason=not mounted
  <name>	reason=not running

one line per stale dir and non-running pod — the dirs first, in the
order read, then the pods. Exited containers are reported but do not
by themselves make the verdict `stale`: a pod with nothing running in
it is what the dl360 incident calls stale, and is already named by
"pods not running".

A podman call that fails, or no podman found at all, is noted on its
own line (`podman: <why>`) and the sections it would have filled read
`(none)` — the dirs section still reads, and stays an honest `stale`
reading of the dirs alone rather than failing the script. jq is
required always.

$PODMAN (default `podman`) is the binary; $MESH_HOME_ROOT (default
${TMPDIR:-/tmp}) is where mesh-home-* directories are looked for —
both overridable so a test can point them at a fake podman script and
fixture directories instead of the real ones.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --help | -h) usage; exit 0 ;;
    *) echo "mesh-home-reading: unexpected argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

command -v jq >/dev/null 2>&1 || { echo "mesh-home-reading: jq is not on PATH" >&2; exit 1; }

podman="${PODMAN:-podman}"
home_root="${MESH_HOME_ROOT:-${TMPDIR:-/tmp}}"

podman_runnable=1
command -v "$podman" >/dev/null 2>&1 || [[ -x "$podman" ]] || podman_runnable=0

# notes_file carries the notes podman_json records: it is called
# through command substitution (to capture its JSON on stdout), which
# runs it in a subshell, so a plain array it appended to would vanish
# with that subshell — a file is the one channel that survives it.
notes_file=$(mktemp)
trap 'rm -f "$notes_file"' EXIT

# podman_json <args...>: the JSON array <podman> <args...> prints, or
# "[]" with a line appended to $notes_file when the call fails or
# does not answer JSON.
podman_json() {
  local out
  if ! out=$("$podman" "$@" 2>&1); then
    echo "podman $*: $out" >>"$notes_file"
    echo '[]'
    return
  fi
  if ! jq -e . >/dev/null 2>&1 <<<"$out"; then
    echo "podman $*: did not answer JSON: $(head -c 200 <<<"$out" | tr '\n' ' ')" >>"$notes_file"
    echo '[]'
    return
  fi
  printf '%s' "$out"
}

if [[ $podman_runnable -eq 0 ]]; then
  echo "podman: $podman not found" >>"$notes_file"
  ps_json='[]'
  pod_json='[]'
  exited_json='[]'
else
  ps_json=$(podman_json ps --format json)
  pod_json=$(podman_json pod ps --format json)
  exited_json=$(podman_json ps -a --filter status=exited --format json)
fi

mapfile -t notes <"$notes_file"

mapfile -t mounted_list < <(jq -r '.[] | .Mounts[]?' <<<"$ps_json" 2>/dev/null || true)

is_mounted() {
  local path=$1 m
  for m in "${mounted_list[@]}"; do
    [[ $m == "$path" ]] && return 0
  done
  return 1
}

shopt -s nullglob
dirs=("$home_root"/mesh-home-*)
shopt -u nullglob

dir_count=${#dirs[@]}
total_bytes=0
stale_dirs=()

echo "== mesh-home dirs"
if [[ $dir_count -eq 0 ]]; then
  echo "(none)"
else
  mounted_count=0
  for d in "${dirs[@]}"; do
    sz=$(du -sb -- "$d" 2>/dev/null | cut -f1)
    [[ -n $sz ]] || sz=0
    total_bytes=$((total_bytes + sz))
    if is_mounted "$d"; then
      mounted="yes"
      mounted_count=$((mounted_count + 1))
    else
      mounted="no"
      stale_dirs+=("$d")
    fi
    echo -e "$d\tbytes=$sz\tmounted=$mounted"
  done
  echo -e "dirs: $dir_count\tbytes: $total_bytes\tmounted: $mounted_count/$dir_count"
fi

mapfile -t pod_lines < <(jq -r '.[] | select(.Name != null and (.Name | startswith("mesh-"))) | "\(.Name)\t\(.Status)"' <<<"$pod_json" 2>/dev/null || true)

running_pods=()
notrunning_pods=()
for line in "${pod_lines[@]}"; do
  status=${line#*$'\t'}
  if [[ $status == "Running" ]]; then
    running_pods+=("$line")
  else
    notrunning_pods+=("$line")
  fi
done

echo
echo "== running mesh pods"
if [[ ${#running_pods[@]} -eq 0 ]]; then
  echo "(none)"
else
  for line in "${running_pods[@]}"; do
    echo -e "${line%%$'\t'*}\tstatus=${line#*$'\t'}"
  done
fi

echo
echo "== pods not running"
if [[ ${#notrunning_pods[@]} -eq 0 ]]; then
  echo "(none)"
else
  for line in "${notrunning_pods[@]}"; do
    echo -e "${line%%$'\t'*}\tstatus=${line#*$'\t'}"
  done
fi

mapfile -t exited_lines < <(jq -r '.[] | select(.Names != null and (.Names[0] | startswith("mesh-"))) | "\(.Names[0])\t\(.Pod // "-")\t\(.State)"' <<<"$exited_json" 2>/dev/null || true)

echo
echo "== exited mesh containers"
if [[ ${#exited_lines[@]} -eq 0 ]]; then
  echo "(none)"
else
  for line in "${exited_lines[@]}"; do
    name=${line%%$'\t'*}
    rest=${line#*$'\t'}
    pod=${rest%%$'\t'*}
    status=${rest#*$'\t'}
    echo -e "$name\tpod=$pod\tstatus=$status"
  done
fi

if [[ ${#notes[@]} -gt 0 ]]; then
  echo
  for n in "${notes[@]}"; do
    echo "note: $n"
  done
fi

echo
if [[ ${#stale_dirs[@]} -eq 0 && ${#notrunning_pods[@]} -eq 0 ]]; then
  echo "ok"
else
  echo "stale"
  for d in "${stale_dirs[@]}"; do
    echo -e "$d\treason=not mounted"
  done
  for line in "${notrunning_pods[@]}"; do
    echo -e "${line%%$'\t'*}\treason=not running"
  done
fi

exit 0
