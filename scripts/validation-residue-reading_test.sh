#!/usr/bin/env bash
# A bash test of scripts/validation-residue-reading.sh's --dry fixture
# output (scripts/testdata/validation-residue-reading, merged in item
# 12): runs `--dry` over it and checks the before/after/difference
# lines it prints, and that --help names the --dry fixture invocation.
# Also sources the script (item 15's guard,
# `[[ "${BASH_SOURCE[0]}" == "$0" ]]`, keeps main() from running on
# source) to exercise count_validations' window arithmetic directly,
# since --dry has no bounds to give it, and checks --window's own
# argument validation. No network, no podman, no Go — plain bash and
# the script under test.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$here/validation-residue-reading.sh"
fixture="$here/testdata/validation-residue-reading"

failures=0

# check name expected got: prints a failure with what was expected and
# what came back when they differ, and counts it.
check() {
  local name="$1" expected="$2" got="$3"
  if [ "$expected" != "$got" ]; then
    echo "FAIL: $name"
    echo "  expected: $expected"
    echo "       got: $got"
    failures=$((failures + 1))
  fi
}

# check_contains name haystack needle: like check, but passes when
# needle is found anywhere in haystack.
check_contains() {
  local name="$1" haystack="$2" needle="$3"
  if [[ "$haystack" != *"$needle"* ]]; then
    echo "FAIL: $name"
    echo "  expected to contain: $needle"
    echo "                  got: $haystack"
    failures=$((failures + 1))
  fi
}

set +e
out="$(bash "$script" --dry "$fixture")"
rc=$?
set -e
check "exit code" "0" "$rc"

before_block="$(awk '/^== before$/{f=1; next} /^== after$/{f=0} f' <<<"$out")"
check_contains "before entries" "$before_block" "entries: 2"
check_contains "before bytes" "$before_block" "bytes: 12"
check_contains "before cache line" "$before_block" "validation cache: 75% hits (30/40)"

after_block="$(awk '/^== after$/{f=1; next} /^== difference$/{f=0} f' <<<"$out")"
check_contains "after entries" "$after_block" "entries: 3"
check_contains "after bytes" "$after_block" "bytes: 21"
check_contains "after cache line" "$after_block" "validation cache: 80% hits (48/60)"

diff_block="$(awk '/^== difference$/{f=1; next} f' <<<"$out")"
check_contains "new entries" "$diff_block" "new entries: 1"
check_contains "new bytes" "$diff_block" "new bytes: 9"
check_contains "validations in window" "$diff_block" "validations in window: 2"
# work/goal-a/1 and goal-b/3 have no session, goal-b/4's session pid is
# dead, goal-a/2's state.json names pid 0 (a sandboxed session): live.
check_contains "leftover work directories" "$diff_block" "leftover work directories: 3"

help_out="$(bash "$script" --help)"
check_contains "--help names the --dry fixture invocation" "$help_out" \
  "scripts/validation-residue-reading.sh --dry scripts/testdata/validation-residue-reading"

# source the script for count_validations alone; the BASH_SOURCE guard
# keeps main() (its arg parsing, its --dry and live paths) from running.
# shellcheck source=scripts/validation-residue-reading.sh
source "$script"

tmp_log="$(mktemp)"
trap 'rm -f "$tmp_log"' EXIT

cat >"$tmp_log" <<'LOG'
not a mesh node line at all: attestation recorded zzz ok
mesh node: 2026/10/15 05:00:00 item x/1: attestation recorded a ok
mesh node: 2026/10/15 06:00:00 item x/2: attestation recorded b ok
mesh node: 2026/10/15 06:30:00 item x/3: attestation refused c nope
mesh node: 2026/10/15 06:45:00 item x/3: attestation recorded c ok
mesh node: 2026/10/15 07:00:00 item x/4: attestation recorded d ok
mesh node: 2026/10/15 08:00:00 item x/5: attestation recorded e ok
LOG

check "count_validations with no bounds counts only mesh node: attestation recorded lines" \
  "5" "$(count_validations "$tmp_log")"

check "count_validations since bound is inclusive" \
  "1" "$(count_validations "$tmp_log" "2026/10/15 06:00:00" "2026/10/15 06:00:00")"

check "count_validations until bound is inclusive" \
  "1" "$(count_validations "$tmp_log" "2026/10/15 07:00:00" "2026/10/15 07:00:00")"

check "count_validations skips lines outside the window" \
  "3" "$(count_validations "$tmp_log" "2026/10/15 06:00:00" "2026/10/15 07:00:00")"

check "count_validations on a missing log gives 0" \
  "0" "$(count_validations "$here/no-such-node.log")"

check "leftover_work_dirs on a missing root gives 0" \
  "0" "$(leftover_work_dirs "$here/no-such-work")"

tmp_work="$(mktemp -d)"
trap 'rm -f "$tmp_log"; rm -rf "$tmp_work"' EXIT
mkdir -p "$tmp_work/g/1" "$tmp_work/g/1.session"
printf '{"pid": %d}\n' "$$" >"$tmp_work/g/1.session/state.json"
check "leftover_work_dirs counts no dir whose session's pid is alive" \
  "0" "$(leftover_work_dirs "$tmp_work")"

set +e
window_out="$(bash "$script" --window not-a-number 2>&1)"
window_rc=$?
set -e
check "--window rejects a non-number: exit code" "2" "$window_rc"
check_contains "--window rejects a non-number: message" "$window_out" \
  "--window wants a whole number of minutes, got 'not-a-number'"

if [ "$failures" -eq 0 ]; then
  echo "ok: validation-residue-reading_test.sh"
  exit 0
fi
echo "$failures check(s) failed"
exit 1
