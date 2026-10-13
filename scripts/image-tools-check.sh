#!/usr/bin/env bash
# image-tools-check.sh — the tools an enrolled repo's checks need, run
# inside the node image (docs/e2e-sandbox.md, Tools check; ADR-007).
#
#   scripts/image-tools-check.sh [fixture-dir]
#
# Run as the session's non-root user, it checks:
#   node   node --version, npm --version, and `npm ci && npx tsc
#          --version` in a copy of the fixture package
#          (images/node/testdata/tools-fixture by default);
#   postgres  initdb a temporary data dir, start a server on a unix
#          socket under it, `psql -c 'select 1'`, stop it;
#   chrome    `<browser> --headless --version`, then a headless start
#          that dumps a data: URL's DOM.
#
# Each prints one `PASS <check>: …` or `FAIL <check>: …` line; the exit
# status is 1 when any failed, else 0. TestImageToolsE2E
# (internal/sandbox/tools_e2e_test.go) runs it in the built image.
set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
fixture="${1:-$here/../images/node/testdata/tools-fixture}"
work="$(mktemp -d "${TMPDIR:-/tmp}/image-tools-check.XXXXXX")"
pgdata="$work/pg"
failed=0

cleanup() {
  if [ -f "$pgdata/postmaster.pid" ] && command -v pg_ctl >/dev/null 2>&1; then
    pg_ctl -D "$pgdata" -m immediate -w stop >/dev/null 2>&1
  fi
  rm -rf "$work"
}
trap cleanup EXIT

pass() { echo "PASS $1: $2"; }
fail() { echo "FAIL $1: $2"; failed=1; }
# last prints a command's output's last line, for a FAIL's reason.
last() { tail -n 3 "$1" | tr '\n' ' ' | sed 's/ *$//'; }

# node: node, npm, and a lockfile install that runs a package's binary.
check_node() {
  local log="$work/node.log" nv npmv tscv
  if ! nv="$(node --version 2>"$log")"; then fail node "node --version: $(last "$log")"; return; fi
  if ! npmv="$(npm --version 2>"$log")"; then fail node "npm --version: $(last "$log")"; return; fi
  if [ ! -f "$fixture/package-lock.json" ]; then fail node "no fixture at $fixture"; return; fi
  mkdir -p "$work/fixture"
  cp "$fixture/package.json" "$fixture/package-lock.json" "$work/fixture/"
  if ! (cd "$work/fixture" && npm ci --no-fund --no-audit) >"$log" 2>&1; then
    fail node "node $nv, npm $npmv; npm ci: $(last "$log")"; return
  fi
  if ! tscv="$(cd "$work/fixture" && npx --no-install tsc --version 2>"$log")"; then
    fail node "node $nv, npm $npmv; npx tsc --version: $(last "$log")"; return
  fi
  pass node "node $nv, npm $npmv, npm ci && npx tsc --version: $tscv"
}

# postgres: a server the session's own user starts in a temp data dir.
check_postgres() {
  local log="$work/pg.log" bin out
  for bin in initdb pg_ctl psql; do
    command -v "$bin" >/dev/null 2>&1 || { fail postgres "$bin not on PATH"; return; }
  done
  if ! initdb -D "$pgdata" -U postgres -A trust --no-sync >"$log" 2>&1; then
    fail postgres "initdb: $(last "$log")"; return
  fi
  if ! pg_ctl -D "$pgdata" -l "$work/pg-server.log" -w -t 30 \
      -o "-k $pgdata -c listen_addresses=''" start >"$log" 2>&1; then
    fail postgres "pg_ctl start: $(last "$work/pg-server.log")"; return
  fi
  if ! out="$(psql -h "$pgdata" -U postgres -d postgres -XAtc 'select 1' 2>"$log")" || [ "$out" != 1 ]; then
    fail postgres "psql -c 'select 1': ${out:-$(last "$log")}"; return
  fi
  pass postgres "$(postgres --version), select 1 = $out as $(id -un)"
}

# chrome: a headless browser that answers its version and starts.
check_chrome() {
  local log="$work/chrome.log" bin="" b v dom
  for b in chromium chromium-browser google-chrome chrome; do
    if command -v "$b" >/dev/null 2>&1; then bin="$b"; break; fi
  done
  if [ -z "$bin" ]; then fail chrome "no chromium, chromium-browser, google-chrome or chrome on PATH"; return; fi
  if ! v="$("$bin" --headless --version 2>"$log" | sed "s/ *$//")"; then
    fail chrome "$bin --headless --version: $(last "$log")"; return
  fi
  # --no-sandbox: the pod is the sandbox, and a rootless pod has no
  # user namespace left for Chrome's own.
  if ! dom="$(timeout 60 "$bin" --headless --no-sandbox --disable-gpu --disable-dev-shm-usage \
      --user-data-dir="$work/chrome-profile" \
      --dump-dom 'data:text/html,<p>mesh-headless-ok</p>' 2>"$log")" \
      || [[ "$dom" != *mesh-headless-ok* ]]; then
    fail chrome "$v; $bin --headless --dump-dom: $(last "$log")"; return
  fi
  pass chrome "$v; $bin --headless --dump-dom rendered a page"
}

check_node
check_postgres
check_chrome
exit "$failed"
