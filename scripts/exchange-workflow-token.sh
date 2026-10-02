#!/usr/bin/env bash
# exchange-workflow-token.sh [repository] [binding]
#
# Exchanges this GitHub Actions run's OIDC identity for a short-lived,
# scoped GitHub token through Rigger, the runsheet gateway's credential
# broker (docs/adr/adr-015-workflow-tokens-exchanged-through-rigger.md;
# runsheet goal rigger-binds-a-workflow-to-a-scoped-github-token). No
# stored token is involved: release-tag.yml's mirror step calls this
# for a contents:write token on runsheet/mesh instead of a secret.
#
# 1. GET "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=runsheet-rigger" with
#    bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN — the run's OIDC JWT is
#    `.value` (the job needs `permissions: id-token: write`).
# 2. POST ${RUNSHEET_GATEWAY:-https://api.runsheet.dev}/graphql with
#    `Authorization: Bearer <jwt>` and the mutation
#    exchangeWorkflowToken(repository, binding) — the exact body is
#    scripts/testdata/exchange-workflow-token/request.json, the
#    contract the runsheet half copies. The platform checks the JWT's
#    claims against the binding file the target repository carries
#    (.runsheet/bindings/<binding>.yaml) and journals the exchange.
#
# Prints the token on stdout and its expiresAt on stderr. A refusal, a
# platform without the mutation, or no OIDC token exits 1 with the
# reason on stderr, one line, prefixed "exchange-workflow-token: ".
# A refusal (the gateway answers with errors) adds one more line after
# it, `presented: kid=… iss=… aud=… sub=… repository=… ref=…
# job_workflow_ref=…`: the JWT's header kid and claims, decoded
# unverified, so the run log shows the runsheet half exactly what to
# accept — never the token, a segment of it or its signature.
#
# repository defaults to runsheet/mesh, binding to mesh-cli-release.
# Needs curl and jq on PATH.
set -euo pipefail

repository=${1:-runsheet/mesh}
binding=${2:-mesh-cli-release}
gateway=${RUNSHEET_GATEWAY:-https://api.runsheet.dev}
audience=runsheet-rigger

die() {
  echo "exchange-workflow-token: $*" >&2
  exit 1
}

# b64url_json <segment>: one base64url JWT segment decoded, unverified.
b64url_json() {
  local s=${1//-/+}
  s=${s//_//}
  case $((${#s} % 4)) in
    2) s+="==" ;;
    3) s+="=" ;;
  esac
  base64 -d <<<"$s" 2>/dev/null
}

# presented <jwt>: the line naming what the token claims, for a refusal.
presented() {
  local header payload rest
  header=$(b64url_json "${1%%.*}") || header=""
  rest=${1#*.}
  payload=$(b64url_json "${rest%%.*}") || payload=""
  jq -rn --argjson h "${header:-null}" --argjson p "${payload:-null}" '
    def v: if . == null then "" elif type == "array" then join(",") else tostring end;
    "presented: kid=\($h.kid | v) iss=\($p.iss | v) aud=\($p.aud | v) sub=\($p.sub | v) repository=\($p.repository | v) ref=\($p.ref | v) job_workflow_ref=\($p.job_workflow_ref | v)"
  ' 2>/dev/null || echo "presented: (the OIDC token's claims could not be decoded)"
}

[[ -n ${ACTIONS_ID_TOKEN_REQUEST_URL:-} && -n ${ACTIONS_ID_TOKEN_REQUEST_TOKEN:-} ]] ||
  die "no OIDC token: ACTIONS_ID_TOKEN_REQUEST_URL/TOKEN unset (the job needs permissions id-token: write)"

oidc_response=$(curl -sS \
  -H "Authorization: Bearer ${ACTIONS_ID_TOKEN_REQUEST_TOKEN}" \
  "${ACTIONS_ID_TOKEN_REQUEST_URL}&audience=${audience}") ||
  die "no OIDC token: the request for audience ${audience} failed"
jwt=$(jq -er '.value // empty' <<<"$oidc_response" 2>/dev/null) ||
  die "no OIDC token: the response for audience ${audience} has no .value"

query=$(printf 'mutation { exchangeWorkflowToken(repository: "%s", binding: "%s") { token expiresAt repository permissions } }' \
  "$repository" "$binding")
body=$(jq -cn --arg query "$query" '{query: $query}')

response=$(curl -sS -X POST \
  -H "Authorization: Bearer ${jwt}" \
  -H "Content-Type: application/json" \
  --data-binary "$body" \
  "${gateway%/}/graphql") ||
  die "the gateway ${gateway} did not answer"

jq -e . >/dev/null 2>&1 <<<"$response" ||
  die "the gateway answered with no JSON: $(head -c 200 <<<"$response" | tr '\n' ' ')"

errors=$(jq -r '[.errors[]?.message] | join("; ")' <<<"$response")
if [[ -n $errors ]]; then
  echo "exchange-workflow-token: refused: ${errors}" >&2
  presented "$jwt" >&2
  exit 1
fi

token=$(jq -er '.data.exchangeWorkflowToken.token // empty' <<<"$response") ||
  die "refused: the answer carries no token"
expires=$(jq -r '.data.exchangeWorkflowToken.expiresAt // "unknown"' <<<"$response")

echo "$expires" >&2
printf '%s\n' "$token"
