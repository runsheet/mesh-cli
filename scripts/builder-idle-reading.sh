#!/usr/bin/env bash
# Takes the reading item 3 of a-builder-tests-what-it-touched-and-never-waits-idle
# owes: over a window (default the last 24 hours), how many Bash tool
# calls in build sessions' transcripts still ran `go test ./...` — the
# whole suite internal/harness/testguard.go's PreToolUse hook now
# refuses before the agent runs it (that goal's item 1) — and the
# median build session's wall-clock minutes, from node.log's own
# `<label>: usage build {…}` lines (internal/node/credentials.go,
# SessionAccount.recordLine). The goal's own acceptance — a count of
# zero and a median under 15 minutes, read the day after release on
# dl360-anythink — is exactly these two numbers.
#
# Like evaluate-once-reading.sh and validation-residue-reading.sh, this
# is a measurement (ADR-020's `Measure:` item): it exits 0 and prints
# what the node's own state shows for a human to read, never a verdict
# a build is judged on.
#
# A build session's transcript is read from wherever this node still
# keeps it: a live `<goal>/<seq>.session/output.jsonl` (ADR-008), a kept
# Codex session's `kept-sessions/<name>/output.jsonl` (harness's
# KeepSessionOutput), or a kept build's own `resume/<item>/` transcript
# (ADR-030, ResumeStore — a resume record is always a build session's).
# A build the node has already swept — ADR-008's work sweep removes a
# merged item's `.session` dir, and a resume record ages out after
# ResumeKeepFor, 7 days — is not counted: this reading is a lower bound
# on what ran, not an audit of every build session that ever did.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: scripts/builder-idle-reading.sh [--since TIME] [--until TIME] [--root ROOT] [--log LOG] [--help]
       scripts/builder-idle-reading.sh --dry [FIXTURE-DIR]

Over a window (default the last 24 hours; --since/--until take
node.log's own `YYYY/MM/DD HH:MM:SS` format, local time), prints:

  whole-suite calls: <n>
    Bash tool calls, across build sessions' transcripts whose session
    started in the window, whose command runs `go test ./...` (any
    flags, inside &&/;/pipes/subshells, a bare `./...` target) — what
    the test-guard hook now refuses before the agent runs it.

  median build minutes: <n>
    the median wall-clock minutes (wallMs ÷ 60000) of every `usage
    build` line in node.log whose timestamp falls in the window.

--root is where build session directories are looked for (default
$MESH_HOME, searched recursively for `*.session` dirs, `kept-sessions/*`
and `resume/*`); --log is the node.log read for usage lines (default
$MESH_HOME/node.log). $MESH_HOME defaults to ~/.mesh.

--dry [FIXTURE-DIR] runs over a fixture instead of live state, taking no
window: FIXTURE-DIR/node.log stands in for the window's node.log (every
`usage build` line in it counts) and each directory under
FIXTURE-DIR/sessions/ stands in for a build session directory — a
state.json naming its kind and an output.jsonl — read the same way as a
live one, with no window (the fixture is already the window).
FIXTURE-DIR defaults to this script's own
testdata/builder-idle-reading/, the fixture this repository carries, so

  bash scripts/builder-idle-reading.sh --dry

prints the two lines above for it.

Needs jq on PATH. The numbers this prints are not a gate (ADR-020): a
reading, never a build's verdict. Exits 0 always, even with nothing
found.
EOF
}

since=""
until_=""
root=""
log=""
dry_dir=""

while [ $# -gt 0 ]; do
  case "$1" in
    --help | -h) usage; exit 0 ;;
    --since) since="$2"; shift 2 ;;
    --since=*) since="${1#--since=}"; shift ;;
    --until) until_="$2"; shift 2 ;;
    --until=*) until_="${1#--until=}"; shift ;;
    --root) root="$2"; shift 2 ;;
    --root=*) root="${1#--root=}"; shift ;;
    --log) log="$2"; shift 2 ;;
    --log=*) log="${1#--log=}"; shift ;;
    --dry)
      if [ $# -ge 2 ] && [[ "$2" != -* ]]; then
        dry_dir="$2"; shift 2
      else
        dry_dir="__default__"; shift
      fi
      ;;
    --dry=*) dry_dir="${1#--dry=}"; shift ;;
    -*) echo "builder-idle-reading: unknown argument: $1" >&2; usage >&2; exit 2 ;;
    *) echo "builder-idle-reading: unexpected argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [ "$dry_dir" = "__default__" ]; then
  script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  dry_dir="$script_dir/testdata/builder-idle-reading"
fi

if ! command -v jq >/dev/null 2>&1; then
  echo "builder-idle-reading: jq is not on PATH" >&2
  exit 2
fi

# is_whole_suite cmd: true when any statement of cmd (split on
# &&, ||, ;, | and subshell parens, as testguard.go's cmdStatementSepRE
# does) runs `go test` against a bare `./...` target.
is_whole_suite() {
  local cmd="$1" stmt
  while IFS= read -r stmt; do
    stmt="$(printf '%s' "$stmt" | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//')"
    [ -n "$stmt" ] || continue
    if printf '%s' "$stmt" | grep -Eq '(^|[^[:alnum:]_])go[[:space:]]+test([^[:alnum:]_]|$)' \
      && printf '%s' "$stmt" | grep -Eq '(^|[[:space:]])\./\.\.\.([[:space:]]|$)'; then
      return 0
    fi
  done < <(printf '%s\n' "$cmd" | sed -E 's/&&|\|\||;|\||\(|\)/\n/g')
  return 1
}

# count_whole_suite_calls file: how many Bash tool_use blocks in the
# Claude Code stream file (one JSON event per line) run a whole-suite
# `go test ./...`.
count_whole_suite_calls() {
  local f="$1" n=0 cmd
  [ -f "$f" ] || { echo 0; return 0; }
  while IFS= read -r cmd; do
    is_whole_suite "$cmd" && n=$((n + 1))
  done < <(jq -r '
    select(.type == "assistant") | .message.content[]? |
    select(.type == "tool_use" and .name == "Bash") | .input.command // empty
  ' "$f" 2>/dev/null)
  echo "$n"
}

# to_epoch_nodelog ts: node.log's own `YYYY/MM/DD HH:MM:SS` as epoch
# seconds, "" when it cannot be parsed.
to_epoch_nodelog() {
  date -d "${1//\//-}" +%s 2>/dev/null || true
}

# to_epoch_rfc3339 ts: an RFC3339 timestamp (state.json's startedAt, a
# resume record's keptAt) as epoch seconds, "" when it cannot be parsed.
to_epoch_rfc3339() {
  [ -n "$1" ] || { echo ""; return 0; }
  date -d "$1" +%s 2>/dev/null || true
}

# median_minutes: reads one wallMs integer per line on stdin and prints
# their median in minutes (one decimal place), or "n/a" given none. Sorts
# with `sort -n` first (portable: mawk has no asort) and takes the
# middle value, averaging the two middle ones on an even count.
median_minutes() {
  sort -n | awk '
    { a[NR] = $1 }
    END {
      if (NR == 0) { print "n/a"; exit }
      if (NR % 2 == 1) {
        m = a[(NR + 1) / 2]
      } else {
        m = (a[NR / 2] + a[NR / 2 + 1]) / 2
      }
      printf "%.1f\n", m / 60000
    }
  '
}

# usage_build_wallms_from_lines: reads node.log lines on stdin already
# known to be in the window, prints one wallMs integer per `usage build`
# line found.
usage_build_wallms_from_lines() {
  while IFS= read -r rest; do
    case "$rest" in
      item\ *': usage build '*)
        json="${rest#*: usage build }"
        jq -r '.wallMs // empty' <<<"$json" 2>/dev/null
        ;;
    esac
  done
}

if [ -n "$dry_dir" ]; then
  if [ ! -d "$dry_dir" ]; then
    echo "builder-idle-reading: --dry directory not found: $dry_dir" >&2
    exit 2
  fi
  echo "== builder-idle-reading (dry run) taken $(date '+%Y-%m-%d %H:%M:%S %z') on $(hostname 2>/dev/null || uname -n), fixture $dry_dir"
  echo

  calls=0
  if [ -d "$dry_dir/sessions" ]; then
    for d in "$dry_dir"/sessions/*/; do
      [ -d "$d" ] || continue
      kind="$(jq -r '.kind // empty' "$d/state.json" 2>/dev/null || true)"
      [ "$kind" = "build" ] || continue
      n="$(count_whole_suite_calls "$d/output.jsonl")"
      calls=$((calls + n))
    done
  fi
  echo "whole-suite calls: $calls"

  wallms=()
  if [ -f "$dry_dir/node.log" ]; then
    while IFS= read -r v; do
      [ -n "$v" ] && wallms+=("$v")
    done < <(LC_ALL=C awk '
      { if (substr($0, 1, 11) == "mesh node: ") print substr($0, 32) }
    ' "$dry_dir/node.log" | usage_build_wallms_from_lines)
  fi
  if [ ${#wallms[@]} -eq 0 ]; then
    echo "median build minutes: n/a"
  else
    echo "median build minutes: $(printf '%s\n' "${wallms[@]}" | median_minutes)"
  fi
  exit 0
fi

home="${MESH_HOME:-$HOME/.mesh}"
root="${root:-$home}"
log="${log:-$home/node.log}"

now_epoch="$(date +%s)"
if [ -n "$since" ]; then
  since_epoch="$(to_epoch_nodelog "$since")"
  if [ -z "$since_epoch" ]; then
    echo "builder-idle-reading: --since not parseable: $since" >&2
    exit 2
  fi
else
  since_epoch=$((now_epoch - 86400))
fi
if [ -n "$until_" ]; then
  until_epoch="$(to_epoch_nodelog "$until_")"
  if [ -z "$until_epoch" ]; then
    echo "builder-idle-reading: --until not parseable: $until_" >&2
    exit 2
  fi
else
  until_epoch="$now_epoch"
fi

echo "== builder-idle-reading taken $(date '+%Y-%m-%d %H:%M:%S %z') on $(hostname 2>/dev/null || uname -n)"
echo "root: $root"
echo "log: $log"
echo "since: ${since:-$(date -d "@$since_epoch" '+%Y/%m/%d %H:%M:%S')}"
echo "until: ${until_:-$(date -d "@$until_epoch" '+%Y/%m/%d %H:%M:%S')}"
echo

# session_dirs: every live `*.session` dir, every `kept-sessions/*` dir
# and every `resume/*` dir under root (ADR-034's org roots carry all
# three under $home/orgs/<org>/ too, so a plain recursive find under
# root sees them without knowing the org names).
session_dirs=()
while IFS= read -r -d '' d; do session_dirs+=("$d"); done < <(find "$root" -type d -name '*.session' -print0 2>/dev/null)
while IFS= read -r -d '' p; do
  while IFS= read -r -d '' d; do session_dirs+=("$d"); done < <(find "$p" -mindepth 1 -maxdepth 1 -type d -print0 2>/dev/null)
done < <(find "$root" -type d -name 'kept-sessions' -print0 2>/dev/null)

resume_dirs=()
while IFS= read -r -d '' p; do
  while IFS= read -r -d '' d; do resume_dirs+=("$d"); done < <(find "$p" -mindepth 1 -maxdepth 1 -type d -print0 2>/dev/null)
done < <(find "$root" -type d -name 'resume' -print0 2>/dev/null)

calls=0
if [ ${#session_dirs[@]} -gt 0 ]; then
  for d in "${session_dirs[@]}"; do
    [ -f "$d/state.json" ] || continue
    kind="$(jq -r '.kind // empty' "$d/state.json" 2>/dev/null || true)"
    [ "$kind" = "build" ] || continue
    started="$(jq -r '.startedAt // empty' "$d/state.json" 2>/dev/null || true)"
    started_epoch="$(to_epoch_rfc3339 "$started")"
    [ -n "$started_epoch" ] || continue
    [ "$started_epoch" -ge "$since_epoch" ] && [ "$started_epoch" -le "$until_epoch" ] || continue
    n="$(count_whole_suite_calls "$d/output.jsonl")"
    calls=$((calls + n))
  done
fi
if [ ${#resume_dirs[@]} -gt 0 ]; then
  for d in "${resume_dirs[@]}"; do
    [ -f "$d/record.json" ] || continue
    started="$(jq -r '.keptAt // empty' "$d/record.json" 2>/dev/null || true)"
    started_epoch="$(to_epoch_rfc3339 "$started")"
    [ -n "$started_epoch" ] || continue
    [ "$started_epoch" -ge "$since_epoch" ] && [ "$started_epoch" -le "$until_epoch" ] || continue
    transcript="$(jq -r '.transcript // empty' "$d/record.json" 2>/dev/null || true)"
    [ -n "$transcript" ] || continue
    n="$(count_whole_suite_calls "$transcript")"
    calls=$((calls + n))
  done
fi
echo "whole-suite calls: $calls"

wallms=()
if [ -f "$log" ]; then
  while IFS= read -r v; do
    [ -n "$v" ] && wallms+=("$v")
  done < <(LC_ALL=C awk -v since="$since_epoch" -v until="$until_epoch" '
    {
      if (substr($0, 1, 11) != "mesh node: ") next
      ts = substr($0, 12, 19)
      cmd = "date -d \"" ts "\" +%s"
      cmd | getline epoch
      close(cmd)
      if (epoch == "" ) next
      if (epoch + 0 < since + 0) next
      if (epoch + 0 > until + 0) next
      print substr($0, 32)
    }
  ' "$log" | usage_build_wallms_from_lines)
else
  echo "log: $log not found" >&2
fi
if [ ${#wallms[@]} -eq 0 ]; then
  echo "median build minutes: n/a"
else
  echo "median build minutes: $(printf '%s\n' "${wallms[@]}" | median_minutes)"
fi

exit 0
