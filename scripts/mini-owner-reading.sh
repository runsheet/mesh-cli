#!/usr/bin/env bash
# Takes the reading item 5 of the-mini-serves-its-enterprise-and-nothing-else
# owes: after the Mini is upgraded and re-registered onto the owner
# contract (goal the-node-serves-its-owners-orgs), is grapevine — an org
# of Chris's, no enterprise's — still carrying a live Mini row, how many
# live Mini rows nodeIndexs has by owner and their ownerKind, and whether
# sessions still claim on anythink's orgs. Before this, answering any of
# the three took a person reading the console or the gateway by hand.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: RUNSHEET_TOKEN=<token> scripts/mini-owner-reading.sh [--now WHEN] [gateway] [slug] [grapevine-org-id] [--help]

Runs three GraphQL reads against <gateway> (default
https://api.runsheet.dev/graphql) on the bearer in $RUNSHEET_TOKEN — an
rs_* console token, or the Mini's own ~/.mesh/token bearer — and prints
one line per reading:

  grapevine: <n> live Mini row(s)
      nodeIndexs where slug is <slug> (default runsheet-mini) and orgId
      is the grapevine org id (the third argument; when omitted,
      resolved from myOrgs by tenant slug or login "grapevine"), live
      meaning status is not "retired".

  by owner: <n> live row(s), ownerKind <kind>, ownerId <id>
      one line per distinct (ownerId, ownerKind) among every live Mini
      row (nodeIndexs where slug is <slug>, across every org); zero
      live rows prints one line naming 0. The live schema is asked for
      ownerKind first; a platform whose NodeIndex does not yet serve it
      (goal registernode-answers-with-the-node-it-made) gets the read
      without it, and every line's ownerKind reads "not served" instead
      of failing.

  claims on anythink orgs: <n> in the last 24h
      work items in review or running, whose orgId is one of the
      anythink enterprise's orgs (enterpriseIndexs, matched by slug or
      tenantSlug "anythink"), and whose firstClaimedAt is within the
      24 hours before --now (default: now).

--now sets the clock the 24h window is measured from (anything `date
-d` takes), for a reproducible test; it does not affect the other two
readings.

Exits non-zero with this usage line when RUNSHEET_TOKEN is unset, and
non-zero naming the reason for any other failure (a refused query, an
org or enterprise that cannot be resolved) — a reading that cannot be
taken is not silently skipped.

Needs curl and jq on PATH, and GNU date (date -d).
EOF
}

now=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --help | -h) usage; exit 0 ;;
    --now) now=$2; shift 2 ;;
    --now=*) now=${1#--now=}; shift ;;
    --) shift; break ;;
    -*) echo "mini-owner-reading: unknown option: $1" >&2; usage >&2; exit 2 ;;
    *) break ;;
  esac
done

if [[ -z ${RUNSHEET_TOKEN:-} ]]; then
  echo "mini-owner-reading: RUNSHEET_TOKEN is not set" >&2
  usage >&2
  exit 2
fi

gateway=${1:-https://api.runsheet.dev/graphql}
slug=${2:-runsheet-mini}
grapevine_org_id=${3:-}
enterprise_slug=anythink

die() {
  echo "mini-owner-reading: $*" >&2
  exit 1
}

# gql_arg <value>: <value> as a quoted, escaped GraphQL string literal —
# GraphQL strings follow JSON's escaping, so @json does it exactly.
gql_arg() { jq -nr --arg v "$1" '$v | @json'; }

# post <query>: the raw gateway response to one GraphQL query; never dies.
post() {
  local body
  body=$(jq -cn --arg q "$1" '{query: $q}')
  curl -sS -X POST \
    -H "Authorization: Bearer ${RUNSHEET_TOKEN}" \
    -H "Content-Type: application/json" \
    --data-binary "$body" \
    "$gateway"
}

# errors_of <response>: the GraphQL errors' messages, joined, or empty.
errors_of() {
  jq -r '[.errors[]?.message] | join("; ")' <<<"$1"
}

# gql <query>: post <query>, dying on a transport failure, an answer
# that is not JSON, or a GraphQL error.
gql() {
  local response
  response=$(post "$1") || die "the gateway $gateway did not answer"
  jq -e . >/dev/null 2>&1 <<<"$response" ||
    die "the gateway answered with no JSON: $(head -c 200 <<<"$response" | tr '\n' ' ')"
  local errors
  errors=$(errors_of "$response")
  [[ -z $errors ]] || die "refused: $errors"
  printf '%s' "$response"
}

if [[ -z $grapevine_org_id ]]; then
  orgs_resp=$(gql '{ myOrgs }')
  grapevine_org_id=$(jq -r '.data.myOrgs[]? | select(((.tenantSlug // "") == "grapevine") or ((.login // "") == "grapevine")) | .id' <<<"$orgs_resp" | head -n1)
  [[ -n $grapevine_org_id ]] || die "no org named 'grapevine' in myOrgs; pass the grapevine org id explicitly"
fi

# nodeIndexs by slug, across every org — read 1 (grapevine) and read 2
# (by owner) both come from this one answer. ownerKind is asked for
# first and dropped on "Cannot query field" alone, never any other
# refusal.
node_query() {
  printf '{ nodeIndexs(namespace: "runsheet", where: [{field: "slug", op: EQ, value: %s}]) { id status orgId ownerId%s } }' \
    "$(gql_arg "$slug")" "${1:-}"
}

owner_kind_served=true
node_resp=$(post "$(node_query ' ownerKind')") || die "the gateway $gateway did not answer"
jq -e . >/dev/null 2>&1 <<<"$node_resp" ||
  die "the gateway answered with no JSON: $(head -c 200 <<<"$node_resp" | tr '\n' ' ')"
node_errors=$(errors_of "$node_resp")
if [[ -n $node_errors ]]; then
  if [[ $node_errors == *"Cannot query field"* && $node_errors == *"ownerKind"* ]]; then
    owner_kind_served=false
    node_resp=$(gql "$(node_query)")
  else
    die "refused: $node_errors"
  fi
fi

grapevine_count=$(jq -r --arg org "$grapevine_org_id" '
  [.data.nodeIndexs[] | select(.orgId == $org) | select(((.status // "") | ascii_downcase) != "retired")] | length
' <<<"$node_resp")
echo "grapevine: $grapevine_count live Mini row(s)"

jq -r --argjson served "$owner_kind_served" '
  ([.data.nodeIndexs[] | select(((.status // "") | ascii_downcase) != "retired")]) as $live
  | ($live
      | group_by([.ownerId, (if $served then .ownerKind else null end)])
      | map({
          ownerId: (.[0].ownerId // "none"),
          ownerKind: (if $served then (.[0].ownerKind // "none") else "not served" end),
          n: length
        })
      | sort_by(.ownerId)) as $groups
  | if ($groups | length) == 0
    then "by owner: 0 live row(s), ownerKind n/a, ownerId n/a"
    else ($groups[] | "by owner: \(.n) live row(s), ownerKind \(.ownerKind), ownerId \(.ownerId)")
    end
' <<<"$node_resp"

ent_resp=$(gql '{ enterpriseIndexs(namespace: "runsheet") { id slug tenantSlug orgs { id } } }')
mapfile -t anythink_orgs < <(jq -r --arg s "$enterprise_slug" '
  [.data.enterpriseIndexs[] | select(.slug == $s or .tenantSlug == $s)]
  | (.[0].orgs // [])[] | .id
' <<<"$ent_resp")
[[ ${#anythink_orgs[@]} -gt 0 ]] ||
  die "no enterprise named '$enterprise_slug' (or it has no orgs) in enterpriseIndexs"

items_json=""
for org in "${anythink_orgs[@]}"; do
  for status in review running; do
    where=$(printf '{field: "org_id", op: EQ, value: %s}, {field: "status", op: EQ, value: %s}' "$(gql_arg "$org")" "$(gql_arg "$status")")
    item_resp=$(gql "$(printf '{ workItemIndexs(namespace: "runsheet", where: [%s]) { id status firstClaimedAt nodeId orgId } }' "$where")")
    items_json+=$(jq -c '.data.workItemIndexs[]?' <<<"$item_resp")
    items_json+=$'\n'
  done
done

now_s=$(if [[ -n $now ]]; then date -d "$now" +%s; else date +%s; fi)
cutoff=$(date -u -d "@$((now_s - 24 * 3600))" '+%Y-%m-%dT%H:%M:%SZ')
claim_count=$(printf '%s' "$items_json" | jq -s --arg cutoff "$cutoff" '
  [.[] | select((.firstClaimedAt // "") >= $cutoff)] | length
')
echo "claims on anythink orgs: $claim_count in the last 24h"
