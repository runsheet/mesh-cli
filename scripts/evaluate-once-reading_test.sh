#!/usr/bin/env bash
# Tests scripts/evaluate-once-reading.sh against two fixture node.logs
# modelled on dl360's /home/anythink/.mesh/node.log and
# /home/grapevine/.mesh/node.log for 2026-10-15 06:35-07:10 local, item
# a-goal-is-done-when-its-last-item-merges/1: one item (ship-it/1)
# evaluated once in each log — the double evaluation the goal's
# acceptance is about — and one `not yours` line in each log.
set -euo pipefail
cd "$(dirname "$0")/.."

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

anythink="scripts/testdata/evaluate-once-anythink.log"
grapevine="scripts/testdata/evaluate-once-grapevine.log"
for f in "$anythink" "$grapevine"; do
  [ -f "$f" ] || fail "fixture not found: $f"
done

output=$(./scripts/evaluate-once-reading.sh "$anythink" "$grapevine")
echo "Output:"
echo "$output"
echo ""

echo "$output" | grep -q '^ship-it/1: 2  MORE THAN ONCE$' \
  || fail "ship-it/1 not flagged with count 2:
$output"
echo "ok: ship-it/1 flagged MORE THAN ONCE with count 2"

echo "$output" | grep -q '^ship-it/2: 1$' \
  || fail "ship-it/2 not reported with count 1:
$output"
echo "$output" | grep -q '^ship-it/3: 1$' \
  || fail "ship-it/3 not reported with count 1:
$output"
echo "ok: items evaluated once are not flagged"

echo "$output" | grep -q '^not yours: 2$' \
  || fail "not yours count is not 2:
$output"
echo "ok: not yours count is 2"

# exits 0 on the fixture (a reading, never a gate — ADR-020)
./scripts/evaluate-once-reading.sh "$anythink" "$grapevine" >/dev/null
echo "ok: exits 0 on the fixture"

# exits 0 even with no log readable
./scripts/evaluate-once-reading.sh /nonexistent/node.log >/dev/null
echo "ok: exits 0 with no log found"

echo ""
echo "evaluate-once-reading.sh: PASS"
