#!/usr/bin/env bash
# Tests scripts/withdrawn-account-reading.sh against the fixtures under
# scripts/testdata/withdrawn-account/ with --fixture-dir, so the test
# never touches the network or ~/.mesh/token: an active, allowed label
# (not stale), a deleted one, a disabled one, one whose allowedNodeIds
# name another node, one the index does not know at all, and a clean
# fixture of two active, allowed labels (stale: 0).
set -euo pipefail
cd "$(dirname "$0")/.."

fixtures=scripts/testdata/withdrawn-account

fail() { echo "FAIL: $*" >&2; exit 1; }

for scenario in active-allowed deleted disabled not-allowed unknown clean; do
  [[ -f "$fixtures/$scenario/node.json" ]] || fail "missing fixture $fixtures/$scenario/node.json"
done

bash -n scripts/withdrawn-account-reading.sh || fail "bash -n failed"

[[ -x scripts/withdrawn-account-reading.sh ]] || fail "scripts/withdrawn-account-reading.sh is not executable"

scripts/withdrawn-account-reading.sh --help | grep -q '^usage: scripts/withdrawn-account-reading.sh' ||
  fail "--help prints no usage"
echo "ok: --help prints usage"

# 1. An active, allowed credential: reported, on the platform as
#    active/allowed, and not stale.
out=$(scripts/withdrawn-account-reading.sh --fixture-dir "$fixtures/active-allowed")
echo "$out"
grep -qx $'Good - Account\tholding=ok\tlimits=ok' <<<"$out" || fail "active-allowed: missing the reported line"
grep -qx $'Good - Account\tstatus=active\tallowed=yes\torg=org-1' <<<"$out" || fail "active-allowed: missing the platform line"
grep -qx 'stale: 0' <<<"$out" || fail "active-allowed: expected stale: 0"
echo "ok: active-allowed — not stale"

# 2. A deleted credential: stale, reason deleted.
out=$(scripts/withdrawn-account-reading.sh --fixture-dir "$fixtures/deleted")
echo "$out"
grep -qx $'Temp - Subsciprtion\treason=deleted' <<<"$out" || fail "deleted: missing the stale line"
grep -qx 'stale: 1' <<<"$out" || fail "deleted: expected stale: 1"
echo "ok: deleted — stale: deleted"

# 3. A disabled credential: stale, reason disabled.
out=$(scripts/withdrawn-account-reading.sh --fixture-dir "$fixtures/disabled")
echo "$out"
grep -qx $'claude - shhemail\treason=disabled' <<<"$out" || fail "disabled: missing the stale line"
grep -qx 'stale: 1' <<<"$out" || fail "disabled: expected stale: 1"
echo "ok: disabled — stale: disabled"

# 4. An active credential whose allowedNodeIds name another node:
#    stale, reason not allowed.
out=$(scripts/withdrawn-account-reading.sh --fixture-dir "$fixtures/not-allowed")
echo "$out"
grep -qx $'mesh - max20\tstatus=active\tallowed=no\torg=org-1' <<<"$out" || fail "not-allowed: missing the platform line"
grep -qx $'mesh - max20\treason=not allowed' <<<"$out" || fail "not-allowed: missing the stale line"
grep -qx 'stale: 1' <<<"$out" || fail "not-allowed: expected stale: 1"
echo "ok: not-allowed — stale: not allowed"

# 5. A reported label no credential of the queried org names: stale,
#    reason unknown.
out=$(scripts/withdrawn-account-reading.sh --fixture-dir "$fixtures/unknown")
echo "$out"
grep -qx $'Ghost - Account\treason=unknown' <<<"$out" || fail "unknown: missing the stale line"
grep -qx 'stale: 1' <<<"$out" || fail "unknown: expected stale: 1"
echo "ok: unknown — stale: unknown"

# 6. A clean fixture: every reported label is active and allowed.
out=$(scripts/withdrawn-account-reading.sh --fixture-dir "$fixtures/clean")
echo "$out"
grep -qx 'stale: 0' <<<"$out" || fail "clean: expected stale: 0"
grep -q '^Good - Account' <<<"$out" || fail "clean: missing Good - Account"
grep -q '^Temp2 - Subscription' <<<"$out" || fail "clean: missing Temp2 - Subscription"
echo "ok: clean — stale: 0"

# A missing fixture file is a named failure, not a silent skip.
if out=$(scripts/withdrawn-account-reading.sh --fixture-dir /nonexistent 2>&1); then
  fail "a missing fixture dir exited 0"
fi
grep -q 'missing fixture' <<<"$out" || fail "a missing fixture did not name itself: $out"
echo "ok: a missing fixture file fails loudly"

echo "withdrawn-account-reading_test.sh: PASS"
