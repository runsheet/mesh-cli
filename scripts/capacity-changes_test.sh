#!/usr/bin/env bash
# Tests scripts/capacity-changes.sh against a fixture log spanning two days.
set -euo pipefail
cd "$(dirname "$0")/.."

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

fixture="scripts/testdata/capacity-changes-fixture.log"
if [ ! -f "$fixture" ]; then
  fail "fixture not found: $fixture"
fi

# Run the script on the fixture
output=$(./scripts/capacity-changes.sh "$fixture")

# Expected output:
# 2026-10-13 with:
#   - capacity 4→5 (bare, other)
#   - capacity 5→6 (host hot)
#   - capacity 6→5 (host hot)
#   - capacity 5→4 (host hot)
#   - capacity 4→3 (host hot)
#   - capacity 3→2 (host hot)
#   - capacity 2→3 (host quiet)
#   - capacity 3→4 (host quiet)
#   - capacity 4→5 (host quiet)
#   - capacity 5→6 (other - clamped)
#   - capacity 6→5 (other)
#   - capacity 5→4 (host hot)
#   - capacity 4→3 (host quiet)
# Total on 2026-10-13: 13 changes
#   Host hot: capacity 5→6, 6→5, 5→4, 4→3, 3→2, 5→4 = 6
#   Host quiet: capacity 2→3, 3→4, 4→5, 4→3 = 4
#   Other: capacity 4→5 (bare), 5→6 (clamped), 6→5 = 3
# Total: 13

# 2026-10-14 with:
#   - capacity 2→3 (host quiet)
#   - capacity 3→4 (host quiet)
#   - capacity 4→5 (host hot)
#   - capacity 5→6 (host hot)
#   - capacity 6→7 (other - clamped)
#   - capacity 7→6 (host quiet)
#   - capacity 6→5 (other)
# Total on 2026-10-14: 7 changes (2 host hot, 3 host quiet, 2 other)

echo "Output:"
echo "$output"
echo ""

# Check 2026-10-13 line exists and has correct counts
if echo "$output" | grep -q "^2026-10-13"; then
  line=$(echo "$output" | grep "^2026-10-13")
  echo "2026-10-13 line: $line"
  if echo "$line" | grep -q "13 (host hot 6, host quiet 4, other 3)"; then
    echo "ok: 2026-10-13 has correct counts"
  else
    fail "2026-10-13 line does not have expected counts (13 total, 6 host hot, 4 host quiet, 3 other):
$line"
  fi
else
  fail "2026-10-13 line not found in output:
$output"
fi

# Check 2026-10-14 line exists and has correct counts
if echo "$output" | grep -q "^2026-10-14"; then
  line=$(echo "$output" | grep "^2026-10-14")
  echo "2026-10-14 line: $line"
  if echo "$line" | grep -q "7 (host hot 2, host quiet 3, other 2)"; then
    echo "ok: 2026-10-14 has correct counts"
  else
    fail "2026-10-14 line does not have expected counts (7 total, 2 host hot, 3 host quiet, 2 other):
$line"
  fi
else
  fail "2026-10-14 line not found in output:
$output"
fi

echo ""
echo "capacity-changes.sh: PASS"
