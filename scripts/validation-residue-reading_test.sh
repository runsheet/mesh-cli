#!/usr/bin/env bash
# Tests scripts/validation-residue-reading.sh --dry against the fixture
# under scripts/testdata/validation-residue-reading/: a before/ and
# after/ directory (one new file appears, two bytes' worth of content
# grows elsewhere), status-before.txt/status-after.txt standing in for
# `mesh node status`'s output, and a node.log with two `attestation
# recorded` lines and one `attestation refused` (which must not count).
set -euo pipefail
cd "$(dirname "$0")/.."

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

fixture="scripts/testdata/validation-residue-reading"
for f in before after status-before.txt status-after.txt node.log; do
  [ -e "$fixture/$f" ] || fail "fixture not found: $fixture/$f"
done

bash -n scripts/validation-residue-reading.sh || fail "bash -n failed"

scripts/validation-residue-reading.sh --help | grep -q '^usage: scripts/validation-residue-reading.sh' ||
  fail "--help prints no usage"

output=$(./scripts/validation-residue-reading.sh --dry "$fixture")
echo "Output:"
echo "$output"
echo ""

echo "$output" | grep -q '^entries: 2$' || fail "before entries not 2:
$output"
echo "$output" | grep -q '^entries: 3$' || fail "after entries not 3:
$output"
echo "ok: before/after entry counts printed"

echo "$output" | grep -qx 'validation cache: 75% hits (30/40), 1.2 GiB of 20.0 GiB' ||
  fail "before validation cache line missing:
$output"
echo "$output" | grep -qx 'validation cache: 80% hits (48/60), 1.2 GiB of 20.0 GiB' ||
  fail "after validation cache line missing:
$output"
echo "ok: both validation cache: lines printed"

echo "$output" | grep -qx 'new entries: 1' || fail "new entries not 1:
$output"
echo "ok: the difference is the one new fixture file"

echo "$output" | grep -qx 'validations in window: 2' ||
  fail "validations in window not 2 (attestation refused must not count):
$output"
echo "ok: validations in window counts only attestation recorded lines"

# exits 0 on the fixture (a reading, never a gate — ADR-020)
./scripts/validation-residue-reading.sh --dry "$fixture" >/dev/null
echo "ok: exits 0 on the fixture"

# a missing fixture directory is a clear error, not a silent 0
if ./scripts/validation-residue-reading.sh --dry /nonexistent/fixture-dir >/dev/null 2>/dev/null; then
  fail "--dry on a missing directory exited 0"
fi
echo "ok: --dry on a missing directory exits non-zero"

echo ""
echo "validation-residue-reading_test.sh: PASS"
