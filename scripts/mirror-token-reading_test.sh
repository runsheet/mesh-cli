#!/usr/bin/env bash
# Tests scripts/mirror-token-reading.sh against a fake `gh` on PATH (the
# fake-gh pattern of mirror-release_test.sh) serving three fixture runs
# — exchanged, fallback (the gateway reason seen on 2026-10-02) and one
# whose job never reached the mirror step — from the fixture logs under
# scripts/testdata/mirror-token-reading/, and asserts the four lines.
set -euo pipefail
cd "$(dirname "$0")/.."

fail() { echo "FAIL: $*" >&2; exit 1; }

fixtures=scripts/testdata/mirror-token-reading
for f in exchanged.log fallback.log absent.log; do
  [[ -f $fixtures/$f ]] || fail "missing fixture $fixtures/$f"
done

bash -n scripts/mirror-token-reading.sh || fail "bash -n failed"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
bin="$work/bin"
mkdir -p "$bin"

# Newest first, as `gh run list` orders them: v0.9.3 never reaches the
# step (absent), v0.9.2 falls back, v0.9.1 exchanges.
cat >"$work/runs.json" <<EOF
[
  {"databaseId": 2003, "headBranch": "v0.9.3", "createdAt": "2026-10-02T05:20:00Z", "conclusion": "failure"},
  {"databaseId": 2002, "headBranch": "v0.9.2", "createdAt": "2026-10-02T05:10:00Z", "conclusion": "success"},
  {"databaseId": 2001, "headBranch": "v0.9.1", "createdAt": "2026-10-02T05:00:00Z", "conclusion": "success"}
]
EOF

cat >"$bin/gh" <<FAKE
#!/usr/bin/env bash
set -euo pipefail
{ printf 'gh'; for a in "\$@"; do printf '\\x1f%s' "\$a"; done; printf '\\n'; } >>"\$FAKE_LOG"
repo_root="$(cd "$(dirname "$0")/.." && pwd)"
case "\${1:-}" in
  run)
    case "\${2:-}" in
      list)
        [[ " \$* " == *" --limit \$FAKE_EXPECT_LIMIT "* ]] || { echo "fake gh: unexpected limit in: \$*" >&2; exit 1; }
        [[ " \$* " == *" runsheet/mesh-cli "* ]] || { echo "fake gh: unexpected repo in: \$*" >&2; exit 1; }
        cat "$work/runs.json"
        ;;
      view)
        id="\$3"
        case "\$id" in
          2001) cat "\$repo_root/$fixtures/exchanged.log" ;;
          2002) cat "\$repo_root/$fixtures/fallback.log" ;;
          2003) cat "\$repo_root/$fixtures/absent.log" ;;
          *) echo "fake gh: unknown run id \$id" >&2; exit 1 ;;
        esac
        ;;
      *) echo "fake gh: unknown run subcommand \${2:-}" >&2; exit 1 ;;
    esac
    ;;
  *) echo "fake gh: unknown command \${1:-}" >&2; exit 1 ;;
esac
FAKE
chmod +x "$bin/gh"

export PATH="$bin:$PATH"
export FAKE_LOG="$work/log"
export FAKE_EXPECT_LIMIT=5
: >"$FAKE_LOG"

scripts/mirror-token-reading.sh --help | grep -q '^usage: scripts/mirror-token-reading.sh' ||
  fail "--help prints no usage"

out=$(scripts/mirror-token-reading.sh)
rc=$?
[[ $rc -eq 0 ]] || fail "exited $rc, wanted 0"

echo "$out"

grep -qx $'v0.9.3\t2003\t2026-10-02T05:20:00Z\tabsent\tfailed at Install cosign' <<<"$out" ||
  fail "missing the absent line for v0.9.3"
grep -qx $'v0.9.2\t2002\t2026-10-02T05:10:00Z\tfallback\trefused: unauthenticated: the bearer is a JWT with no role (unknown key id)' <<<"$out" ||
  fail "missing the fallback line with its reason for v0.9.2"
grep -qx $'v0.9.1\t2001\t2026-10-02T05:00:00Z\texchanged\t' <<<"$out" ||
  fail "missing the exchanged line for v0.9.1"
grep -qx 'latest: fallback (v0.9.2, run 2002)' <<<"$out" ||
  fail "missing the latest line naming the newest run that reached the step"

grep -q 'echo "mirror token' <<<"$out" && fail "an echoed source line was read as the reading"

# --limit N is passed through from the count argument.
: >"$FAKE_LOG"
FAKE_EXPECT_LIMIT=2 scripts/mirror-token-reading.sh 2 >/dev/null
grep -q -- '--limit' "$FAKE_LOG" || fail "count argument did not reach --limit"

# A non-numeric count is refused.
if scripts/mirror-token-reading.sh abc >/dev/null 2>"$work/err"; then fail "a non-numeric count exited 0"; fi
grep -q "whole number" "$work/err" || fail "a non-numeric count did not say why"

echo "mirror-token-reading_test.sh: PASS"
