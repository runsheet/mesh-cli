#!/usr/bin/env bash
# mirror-token.sh [repository] [binding]
#
# The token release-tag.yml's "Mirror the release to runsheet/mesh"
# step publishes with, chosen in one place
# (docs/adr/adr-015-workflow-tokens-exchanged-through-rigger.md):
#
# - scripts/exchange-workflow-token.sh exchanges the run's OIDC identity
#   through Rigger for a token on <repository> under <binding>: the token
#   on stdout, `mirror token: exchanged through rigger, expires
#   <expiresAt>` on stderr;
# - a failed exchange takes the transitional RELEASE_TOKEN fallback
#   (the one function below): the secret on stdout, `mirror token:
#   RELEASE_TOKEN fallback (exchange failed: <reason>)` on stderr;
# - with neither, `mirror token: none — the exchange through rigger
#   failed (<reason>) and RELEASE_TOKEN is not set` on stderr, exit 1.
#
# stdout carries the token alone; every other line goes to stderr. A
# refusal's `presented: …` line (the JWT's claims) is passed through on
# its own line, outside the reason. The `mirror token: ` lines are what
# scripts/mirror-token-reading.sh reads from the run logs: their text is
# a contract, never reworded.
#
# repository defaults to runsheet/mesh, binding to mesh-cli-release.
set -euo pipefail

repository=${1:-runsheet/mesh}
binding=${2:-mesh-cli-release}
here=$(cd "$(dirname "$0")" && pwd)

err=$(mktemp)
trap 'rm -f "$err"' EXIT

# The transitional fallback: this read and the function below are all of
# it. The operator retires it by deleting the RELEASE_TOKEN secret
# (`gh secret delete RELEASE_TOKEN --repo runsheet/mesh-cli`) once
# scripts/mirror-token-reading.sh reads a run `exchanged`; a cleanup PR
# deleting them and the one call further down follows that, not before.
release_token=${RELEASE_TOKEN:-}
fallback_token() {
  [[ -n $release_token ]] || return 1
  echo "mirror token: RELEASE_TOKEN fallback (exchange failed: $1)" >&2
  printf '%s\n' "$release_token"
}

if token=$("$here/exchange-workflow-token.sh" "$repository" "$binding" 2>"$err"); then
  echo "mirror token: exchanged through rigger, expires $(tail -n1 "$err")" >&2
  printf '%s\n' "$token"
  exit 0
fi

grep '^presented: ' "$err" >&2 || true
reason=$(grep -v '^presented: ' "$err" | tr '\n' ' ' | sed 's/^exchange-workflow-token: //; s/ *$//')

fallback_token "$reason" && exit 0

echo "mirror token: none — the exchange through rigger failed (${reason}) and RELEASE_TOKEN is not set" >&2
exit 1
