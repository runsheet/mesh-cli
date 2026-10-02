#!/usr/bin/env bash
# Tests scripts/mirror-token.sh against a fake `curl` on PATH (the
# pattern of exchange-workflow-token_test.sh): the real
# exchange-workflow-token.sh runs, its OIDC request answered with a
# fixed JWT and the gateway's POST from the fixtures under
# scripts/testdata/exchange-workflow-token/. Each case asserts the exact
# `mirror token: ` line scripts/mirror-token-reading.sh reads.
set -euo pipefail
cd "$(dirname "$0")/.."

fail() { echo "FAIL: $*" >&2; exit 1; }

fixtures=scripts/testdata/exchange-workflow-token

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
bin="$work/bin"
mkdir -p "$bin"

cat >"$bin/curl" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
url=""
for a in "$@"; do
  case $a in http*://*) url=$a ;; esac
done
case $url in
  "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=runsheet-rigger") echo '{"count":1,"value":"fake.oidc.jwt"}' ;;
  "$RUNSHEET_GATEWAY/graphql") cat "$FAKE_ANSWER" ;;
  *) echo "fake curl: unexpected url $url" >&2; exit 6 ;;
esac
FAKE
chmod +x "$bin/curl"

export PATH="$bin:$PATH"
export ACTIONS_ID_TOKEN_REQUEST_URL="https://actions.example/token?api-version=2.0"
export ACTIONS_ID_TOKEN_REQUEST_TOKEN="fake-request-token"
export RUNSHEET_GATEWAY="https://gateway.example"

exchanged=$(jq -r .data.exchangeWorkflowToken.token "$fixtures/response.json")
refusal=$(jq -r '.errors[0].message' "$fixtures/refused.json")

# exchanged: stdout is the exchanged token and nothing else; stderr is
# the exchanged line with expiresAt. RELEASE_TOKEN set is not used.
export FAKE_ANSWER="$fixtures/response.json"
RELEASE_TOKEN=stored-secret scripts/mirror-token.sh runsheet/mesh mesh-cli-release >"$work/out" 2>"$work/err" ||
  fail "exchanged exited non-zero: $(cat "$work/err")"
[[ $(cat "$work/out") == "$exchanged" ]] || fail "exchanged: stdout is not only the token: '$(cat "$work/out")'"
[[ $(wc -l <"$work/out") -eq 1 ]] || fail "exchanged: stdout has more than one line"
[[ $(cat "$work/err") == "mirror token: exchanged through rigger, expires 2026-09-30T13:00:00Z" ]] ||
  fail "exchanged: stderr is '$(cat "$work/err")'"
echo "ok: exchanged — the token alone on stdout, the exchanged line on stderr"

# fallback: the exchange is refused and RELEASE_TOKEN is set — the
# secret on stdout, the fallback line with the gateway's reason.
export FAKE_ANSWER="$fixtures/refused.json"
RELEASE_TOKEN=stored-secret scripts/mirror-token.sh runsheet/mesh mesh-cli-release >"$work/out" 2>"$work/err" ||
  fail "fallback exited non-zero: $(cat "$work/err")"
[[ $(cat "$work/out") == "stored-secret" ]] || fail "fallback: stdout is not only the secret: '$(cat "$work/out")'"
grep -qxF "mirror token: RELEASE_TOKEN fallback (exchange failed: refused: ${refusal})" "$work/err" ||
  fail "fallback: no fallback line with the reason: $(cat "$work/err")"
grep -q '^presented: ' "$work/err" || fail "fallback: the refusal's presented: line is not passed through: $(cat "$work/err")"
echo "ok: fallback — the secret on stdout, the fallback line with the reason"

# none: the exchange is refused and RELEASE_TOKEN is unset — nothing on
# stdout, the none line, exit 1.
set +e
(unset RELEASE_TOKEN; scripts/mirror-token.sh runsheet/mesh mesh-cli-release >"$work/out" 2>"$work/err")
code=$?
set -e
[[ $code -eq 1 ]] || fail "none: exited $code, want 1"
[[ ! -s $work/out ]] || fail "none: printed '$(cat "$work/out")' on stdout"
grep -qxF "mirror token: none — the exchange through rigger failed (refused: ${refusal}) and RELEASE_TOKEN is not set" "$work/err" ||
  fail "none: no none line: $(cat "$work/err")"
echo "ok: none — exit 1, nothing on stdout, the none line"

# An empty RELEASE_TOKEN (an unset secret renders as "") is none too.
set +e
RELEASE_TOKEN="" scripts/mirror-token.sh >"$work/out" 2>"$work/err"
code=$?
set -e
[[ $code -eq 1 && ! -s $work/out ]] || fail "none (empty secret): exited $code, stdout '$(cat "$work/out")'"
grep -q '^mirror token: none — ' "$work/err" || fail "none (empty secret): no none line: $(cat "$work/err")"
echo "ok: none — an empty RELEASE_TOKEN is none"

echo "mirror-token_test.sh: PASS"
