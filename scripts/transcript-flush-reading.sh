#!/usr/bin/env bash
# Takes the reading item 3 of a-lease-s-transcript-flush-interval-is-
# set-from-the-brief-s-flush_s owes (ADR-020's `Measure:` item): whether
# a lease whose brief names flush_s actually flushes its transcript
# upload that often in practice, not just that
# internal/transcript/transcript.go's flushEvery clamps the number to
# [2s, 15s] (its own unit test, TestFlushEvery, already covers the
# clamp).
#
# Runs internal/transcript/cmd/flushreading, which starts a stand-in
# for the product's upload_url, drives a real
# internal/transcript.Uploader against it with the asked flush_s, and
# times the gap between two consecutive chunks landing: the uploader's
# actual flush period.
#
# Like host-backoff-reading.sh and mcp-auth-reading.sh, this is a
# measurement (ADR-020): it exits 0 and prints a number for a human or
# the evaluator to read, never a verdict a build is judged on — the
# evaluator records it as a READING against the goal's flush_s clause
# (AGENTS.md's host-reading rule), not pass/failed on the figure.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: scripts/transcript-flush-reading.sh --flush-s N [--timeout DURATION] [--help]

Prints one line:

  flush_s=<n>s measured=<n>ms

<n>s is --flush-s as given; <n>ms is the measured gap, in
milliseconds, between two consecutive transcript chunks landing at a
stand-in upload_url driven by a real
internal/transcript.Uploader(loc{FlushS: N}) — the uploader's actual
flush period, which internal/transcript/transcript.go's flushEvery
clamps to [2000ms, 15000ms] (1 and below read as 2, above 15 read as
15). A figure close to N*1000 is expected; this is a reading, not a
gate.

--timeout (default 30s, Go duration syntax) bounds how long the
measurement waits for each chunk before giving up.

$GO (default `go`) is the toolchain run as
`$GO run ./internal/transcript/cmd/flushreading`.
EOF
}

flush_s=""
timeout="30s"
while [ $# -gt 0 ]; do
  case "$1" in
    --help | -h) usage; exit 0 ;;
    --flush-s) flush_s="$2"; shift 2 ;;
    --flush-s=*) flush_s="${1#--flush-s=}"; shift ;;
    --timeout) timeout="$2"; shift 2 ;;
    --timeout=*) timeout="${1#--timeout=}"; shift ;;
    *) echo "transcript-flush-reading: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done
case "$flush_s" in '' | *[!0-9]*) echo "transcript-flush-reading: --flush-s must be a positive whole number, got '$flush_s'" >&2; exit 2 ;; esac

go_bin="${GO:-go}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
exec "$go_bin" run ./internal/transcript/cmd/flushreading --flush-s "$flush_s" --timeout "$timeout"
