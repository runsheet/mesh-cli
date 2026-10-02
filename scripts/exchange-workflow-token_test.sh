#!/usr/bin/env bash
# Tests scripts/exchange-workflow-token.sh against a fake `curl` on PATH
# (the fake-gh pattern of mirror-release_test.sh) that answers the OIDC
# request with a fixed JWT and the gateway's POST from the fixture files
# under scripts/testdata/exchange-workflow-token/ — the contract the
# runsheet half copies. The body the script sends is kept and compared
# with request.json.
set -euo pipefail
cd "$(dirname "$0")/.."

fail() { echo "FAIL: $*" >&2; exit 1; }

fixtures=scripts/testdata/exchange-workflow-token
for f in request.json response.json refused.json unknown-field.json README.md; do
  [[ -f $fixtures/$f ]] || fail "missing fixture $fixtures/$f"
done

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
bin="$work/bin"
mkdir -p "$bin"

cat >"$bin/curl" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
url="" data="" auth="" prev=""
for a in "$@"; do
  case $prev in
    --data-binary) data=$a ;;
    -H) [[ $a == Authorization:* ]] && auth=$a ;;
  esac
  case $a in http*://*) url=$a ;; esac
  prev=$a
done
case $url in
  "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=runsheet-rigger")
    [[ $auth == "Authorization: Bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" ]] || { echo "bad OIDC bearer" >&2; exit 22; }
    jq -cn --arg v "$FAKE_JWT" '{count: 1, value: $v}'
    ;;
  "$RUNSHEET_GATEWAY/graphql")
    [[ $auth == "Authorization: Bearer $FAKE_JWT" ]] || { echo "bad gateway bearer: $auth" >&2; exit 22; }
    printf '%s' "$data" >"$FAKE_SENT"
    cat "$FAKE_ANSWER"
    ;;
  *) echo "fake curl: unexpected url $url" >&2; exit 6 ;;
esac
FAKE
chmod +x "$bin/curl"

export PATH="$bin:$PATH"
export ACTIONS_ID_TOKEN_REQUEST_URL="https://actions.example/token?api-version=2.0"
export ACTIONS_ID_TOKEN_REQUEST_TOKEN="fake-request-token"
export RUNSHEET_GATEWAY="https://gateway.example"
export FAKE_SENT="$work/sent.json"
export FAKE_JWT="fake.oidc.jwt"

# b64url <text>: base64url without padding, as a JWT segment is.
b64url() { printf '%s' "$1" | base64 | tr -d '\n=' | tr '+/' '-_'; }

# 1. Success: the token on stdout, expiresAt on stderr, and the body
#    sent is request.json exactly.
export FAKE_ANSWER="$fixtures/response.json"
out=$(scripts/exchange-workflow-token.sh 2>"$work/err") || fail "success exited non-zero: $(cat "$work/err")"
[[ $out == "$(jq -r .data.exchangeWorkflowToken.token "$fixtures/response.json")" ]] || fail "token not printed, got '$out'"
[[ $(cat "$work/err") == "2026-09-30T13:00:00Z" ]] || fail "expiresAt not on stderr, got '$(cat "$work/err")'"
[[ $(jq -cS . "$FAKE_SENT") == "$(jq -cS . "$fixtures/request.json")" ]] ||
  fail "the body sent differs from request.json: $(cat "$FAKE_SENT")"
jq -e '.query | test("exchangeWorkflowToken\\(repository: \"runsheet/mesh\", binding: \"mesh-cli-release\"\\)")' \
  "$fixtures/request.json" >/dev/null || fail "request.json does not name the mutation, repository and binding"
echo "ok: success — token on stdout, expiresAt on stderr, body is request.json"

# 2. Refused: a binding whose claims do not match.
export FAKE_ANSWER="$fixtures/refused.json"
if out=$(scripts/exchange-workflow-token.sh 2>"$work/err"); then fail "refused exited 0"; fi
[[ -z $out ]] || fail "refused printed a token: $out"
grep -q 'exchange-workflow-token: refused: binding mesh-cli-release on runsheet/mesh refuses this token' "$work/err" ||
  fail "refused did not give the reason: $(cat "$work/err")"
echo "ok: refused — exits non-zero with the platform's reason"

# 2b. Refused: the line after the reason names what the JWT presented —
#     header kid and claims, decoded unverified — and never the token,
#     a segment of it, its signature or a GitHub token.
jwt_header=$(b64url '{"alg":"RS256","typ":"JWT","kid":"1F2AB83404C08EC9EA0BB99DAED02186B091DBF4"}')
jwt_payload=$(b64url '{"iss":"https://token.actions.githubusercontent.com","aud":"runsheet-rigger","sub":"repo:runsheet/mesh-cli:ref:refs/tags/v0.2.235","repository":"runsheet/mesh-cli","ref":"refs/tags/v0.2.235","job_workflow_ref":"runsheet/mesh-cli/.github/workflows/release-tag.yml@refs/tags/v0.2.235"}')
jwt_sig=$(b64url 'not-a-real-signature-bytes')
export FAKE_JWT="${jwt_header}.${jwt_payload}.${jwt_sig}"
if out=$(scripts/exchange-workflow-token.sh 2>"$work/err"); then fail "refused (presented) exited 0"; fi
[[ -z $out ]] || fail "refused (presented) printed a token: $out"
[[ $(sed -n 1p "$work/err") == "exchange-workflow-token: refused: "* ]] || fail "the reason is not the first line: $(cat "$work/err")"
line=$(grep '^presented: kid=' "$work/err") || fail "refused did not print a presented: line: $(cat "$work/err")"
[[ $line == "presented: kid=1F2AB83404C08EC9EA0BB99DAED02186B091DBF4 iss=https://token.actions.githubusercontent.com aud=runsheet-rigger sub=repo:runsheet/mesh-cli:ref:refs/tags/v0.2.235 repository=runsheet/mesh-cli ref=refs/tags/v0.2.235 job_workflow_ref=runsheet/mesh-cli/.github/workflows/release-tag.yml@refs/tags/v0.2.235" ]] ||
  fail "presented line wrong: $line"
for part in iss= aud= sub= repository= ref=; do
  [[ $line == *" $part"* ]] || fail "presented line lacks $part: $line"
done
for secret in "$FAKE_JWT" "$jwt_header" "$jwt_payload" "$jwt_sig" ghs_; do
  if grep -qF -- "$secret" "$work/err"; then fail "refused stderr carries a token or JWT segment ($secret)"; fi
done
echo "ok: refused — a presented: line names kid, iss, aud, sub, repository, ref, job_workflow_ref and no token"
export FAKE_JWT="fake.oidc.jwt"

# 3. Unknown field: a platform without the mutation.
export FAKE_ANSWER="$fixtures/unknown-field.json"
if out=$(scripts/exchange-workflow-token.sh 2>"$work/err"); then fail "unknown-field exited 0"; fi
[[ -z $out ]] || fail "unknown-field printed a token: $out"
grep -q 'Cannot query field "exchangeWorkflowToken" on type "Mutation"' "$work/err" ||
  fail "unknown-field did not give the reason: $(cat "$work/err")"
echo "ok: unknown-field — exits non-zero naming the missing mutation"

# 4. No OIDC token (no id-token: write).
if (unset ACTIONS_ID_TOKEN_REQUEST_URL; scripts/exchange-workflow-token.sh >/dev/null 2>"$work/err"); then fail "no OIDC exited 0"; fi
grep -q 'no OIDC token' "$work/err" || fail "no OIDC did not say so: $(cat "$work/err")"
echo "ok: no OIDC token — exits non-zero"

echo "exchange-workflow-token_test.sh: PASS"
