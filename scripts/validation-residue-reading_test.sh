#!/usr/bin/env bash
# A bash test of scripts/validation-residue-reading.sh's --dry fixture
# output (scripts/testdata/validation-residue-reading, merged in item
# 12): runs `--dry` over it and checks the before/after/difference
# lines it prints, and that --help names the --dry fixture invocation.
# No network, no podman, no Go — plain bash and the script under test.
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

help_out="$(bash "$script" --help)"
check_contains "--help names the --dry fixture invocation" "$help_out" \
  "scripts/validation-residue-reading.sh --dry scripts/testdata/validation-residue-reading"

if [ "$failures" -eq 0 ]; then
  echo "ok: validation-residue-reading_test.sh"
  exit 0
fi
echo "$failures check(s) failed"
exit 1
