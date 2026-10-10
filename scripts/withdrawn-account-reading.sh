#!/usr/bin/env bash
# Takes the reading item 4 of a-withdrawn-account-leaves-the-node owes:
# which labels a node's nodeIndexs holdingAccounts and limits still
# report that its orgs' credentialIndexs do not show as an active
# credential allowed on the node — deleted, disabled, allowed on
# another node, or unknown to the index altogether. Before items 1-3
# landed, a withdrawn credential lingered in these two reports for up
# to 24 hours with no way to see the lag from outside the node (the
# goal's statement, Chris 2026-10-03: a deleted `Temp - Subsciprtion`
# and two disabled accounts still showing as held on the Mini's Node
# page a heartbeat after the platform's own read had stopped naming
# them). After the Mini is upgraded, rerunning this against it is the
# goal's acceptance (4) reading.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: scripts/withdrawn-account-reading.sh [--node SLUG|ID] [--org ORG]... [--gateway URL] [--fixture-dir DIR] [--help]

Reads, for one node, three labelled sections:

  == reported
    every label the node's nodeIndexs row(s) carry under
    holdingAccounts or limits, one line each:
      <label>	holding=<status>	limits=<status>
    "-" where the label is absent from that surface.

  == platform
    every credential (credentialIndexs) of the node's orgs, one line
    each:
      <label>	status=<status>	allowed=<yes|no>	org=<orgId>
    allowed is true when allowedNodeIds is empty, names one of the
    node's own ids, or — a legacy per-org node, ADR 005's amendment —
    names the credential's own orgId.

  == stale
    every reported label that is not an active, allowed credential,
    one line each:
      <label>	reason=<deleted|disabled|not allowed|unknown>
    "unknown" is a label no credential of the queried orgs (or their
    enterprise) carries.

and a final line, `stale: N` (0 when every reported label is active
and allowed). Exits 0 whatever N is: the reading is the deliverable,
never a verdict.

--node (default mini) is the node's slug, or its id when it looks like
a UUID (nodeIndexs filtered by "slug" or "id" EQ). --org (repeatable)
is an org id to scope the read to, for both nodeIndexs (which refuses
a bare slug/id filter: "needs a filter on one of org_id, owner_id, id
that is yours") and credentialIndexs; default is every org myOrgs
names, queried one by one, keeping only the node's non-retired row(s)
when any exist (a slug like "mini" leaves a retired row behind in
nearly every org this token can see) — the orgId(s) of the rows kept
are then the orgs credentials are read for. In live mode each org's own enterprise (its
orgIndex's enterpriseId, when the platform serves one) is read too and
its credentials folded in, the way a node's own claim-time read does
(gateway/credentialindex.go) — an enterprise-scoped credential like
`Temp - Subsciprtion` would otherwise never be matched and read
"unknown" instead of what it really is.

Live mode (the default) reads <gateway> (default
https://api.runsheet.dev/graphql) on the bearer in ~/.mesh/token
($MESH_HOME/token when set): its first line when it is an rs_* console
token, else the accessToken of the Latchkey machine credential JSON
there — the way mesh itself does. It is used as stored; this script
never refreshes it. holdingAccounts is always asked for as `{ account
status }`; `limits` is appended and dropped again when the platform
refuses the field ("Cannot query field" — it is not served on
NodeIndex yet) so an older platform still reads.

--fixture-dir DIR skips the network and the token entirely: it reads

  DIR/node.json                 the nodeIndexs query's response
  DIR/credentials-<ORGID>.json  the credentialIndexs response for org
                                 <ORGID>, one file per org read (the
                                 default orgs, or each --org given) —
                                 write in whatever an enterprise's
                                 credentials would also contribute,
                                 since fixture mode does no separate
                                 enterprise read

both the raw `{"data": {...}}` GraphQL shape, exactly what the gateway
would have answered, so the test never touches the network.

Exits non-zero naming the reason when the token is missing or
unreadable, a query is refused, a node is not found, or a fixture file
named above is missing — a reading that cannot be taken is not
silently skipped.

Needs jq on PATH always, curl too in live mode.
EOF
}

node=mini
orgs=()
gateway=https://api.runsheet.dev/graphql
fixture_dir=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --help | -h) usage; exit 0 ;;
    --node) node=$2; shift 2 ;;
    --node=*) node=${1#--node=}; shift ;;
    --org) orgs+=("$2"); shift 2 ;;
    --org=*) orgs+=("${1#--org=}"); shift ;;
    --gateway) gateway=$2; shift 2 ;;
    --gateway=*) gateway=${1#--gateway=}; shift ;;
    --fixture-dir) fixture_dir=$2; shift 2 ;;
    --fixture-dir=*) fixture_dir=${1#--fixture-dir=}; shift ;;
    -*) echo "withdrawn-account-reading: unknown option: $1" >&2; usage >&2; exit 2 ;;
    *) echo "withdrawn-account-reading: unexpected argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

die() {
  echo "withdrawn-account-reading: $*" >&2
  exit 1
}

command -v jq >/dev/null 2>&1 || die "jq is not on PATH"

# gql_arg <value>: <value> as a quoted, escaped GraphQL string literal —
# GraphQL strings follow JSON's escaping, so @json does it exactly.
gql_arg() { jq -nr --arg v "$1" '$v | @json'; }

# errors_of <response>: the GraphQL errors' messages, joined, or empty.
errors_of() {
  jq -r '[.errors[]?.message] | join("; ")' <<<"$1" 2>/dev/null || true
}

if [[ -z $fixture_dir ]]; then
  command -v curl >/dev/null 2>&1 || die "curl is not on PATH"

  home="${MESH_HOME:-$HOME/.mesh}"
  token_file="$home/token"
  [[ -f $token_file ]] || die "no token at $token_file"
  token="$(head -n1 "$token_file")"
  if [[ $token != rs_* ]]; then
    token="$(jq -r '.accessToken // empty' "$token_file" 2>/dev/null || true)"
  fi
  [[ -n $token ]] || die "$token_file holds no rs_* token or machine accessToken"

  # post <query>: the raw gateway response to one GraphQL query.
  post() {
    local body
    body=$(jq -cn --arg q "$1" '{query: $q}')
    curl -sS -X POST \
      -H "Authorization: Bearer ${token}" \
      -H "Content-Type: application/json" \
      --data-binary "$body" \
      "$gateway"
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
fi

# read_fixture <path>: a fixture file's JSON, dying when it is missing
# or carries GraphQL errors of its own (a refusal fixture).
read_fixture() {
  local path=$1
  [[ -f $path ]] || die "missing fixture $path"
  local response
  response=$(cat "$path")
  local errors
  errors=$(errors_of "$response")
  [[ -z $errors ]] || die "refused: $errors"
  printf '%s' "$response"
}

node_field=slug
[[ $node =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$ ]] && node_field=id

if [[ -n $fixture_dir ]]; then
  node_resp=$(read_fixture "$fixture_dir/node.json")
  node_rows=$(jq -c '.data.nodeIndexs // []' <<<"$node_resp")
else
  # nodeIndexs refuses a bare slug filter ("needs a filter on one of
  # org_id, owner_id, id that is yours"), and a slug like "mini" has a
  # retired row in nearly every org this token can see — so the org(s)
  # to query are found live, not guessed: every --org given, else
  # every org myOrgs names.
  if [[ ${#orgs[@]} -eq 0 ]]; then
    myorgs_resp=$(gql '{ myOrgs }')
    mapfile -t query_orgs < <(jq -r '.data.myOrgs[]?.id' <<<"$myorgs_resp")
  else
    query_orgs=("${orgs[@]}")
  fi
  [[ ${#query_orgs[@]} -gt 0 ]] || die "no org to query nodeIndexs with (myOrgs named none, and no --org given)"

  # node_query <org> <extra-selection>: the nodeIndexs query for one
  # org, holdingAccounts always sub-selected (the platform refuses it
  # bare), limits appended only when asked — the platform may not
  # serve it yet.
  node_query() {
    printf '{ nodeIndexs(namespace: "runsheet", where: [{field: "org_id", op: EQ, value: %s}, {field: %s, op: EQ, value: %s}]) { id slug status orgId version holdingAccounts { account status }%s } }' \
      "$(gql_arg "$1")" "$(gql_arg "$node_field")" "$(gql_arg "$node")" "${2:-}"
  }

  all_rows="[]"
  for org in "${query_orgs[@]}"; do
    resp=$(post "$(node_query "$org" ' limits { account status }')") || die "the gateway $gateway did not answer"
    jq -e . >/dev/null 2>&1 <<<"$resp" ||
      die "the gateway answered with no JSON: $(head -c 200 <<<"$resp" | tr '\n' ' ')"
    errs=$(errors_of "$resp")
    if [[ -n $errs ]]; then
      if [[ $errs == *"Cannot query field"* && $errs == *'"limits"'* ]]; then
        resp=$(gql "$(node_query "$org")")
      else
        die "refused: $errs"
      fi
    fi
    all_rows=$(jq -cn --argjson a "$all_rows" --argjson b "$(jq -c '.data.nodeIndexs // []' <<<"$resp")" '$a + $b')
  done

  if [[ ${#orgs[@]} -eq 0 ]]; then
    # No explicit --org: prefer the non-retired row(s) over the many
    # retired registrations the same slug leaves behind across orgs;
    # fall back to every row found when none is live, so a fully
    # retired node still reads rather than failing outright.
    live_rows=$(jq -c '[.[] | select(((.status // "") | ascii_downcase) != "retired")]' <<<"$all_rows")
    if [[ "$(jq 'length' <<<"$live_rows")" -gt 0 ]]; then
      node_rows=$live_rows
    else
      node_rows=$all_rows
    fi
  else
    node_rows=$all_rows
  fi
fi

[[ "$(jq 'length' <<<"$node_rows")" -gt 0 ]] || die "no nodeIndexs row for $node_field $node"

node_ids=$(jq -c '[.[].id] | unique' <<<"$node_rows")

if [[ ${#orgs[@]} -eq 0 ]]; then
  mapfile -t orgs < <(jq -r '[.[].orgId] | unique | .[]' <<<"$node_rows")
fi
[[ ${#orgs[@]} -gt 0 ]] || die "the node names no org to read credentials for; pass --org"

# org_enterprise <org>: the org's enterpriseId (orgIndex), empty when
# the org has none or an older platform does not serve the field.
org_enterprise() {
  local org=$1 resp errs
  resp=$(post "$(printf '{ orgIndex(namespace: "runsheet", id: %s) { enterpriseId } }' "$(gql_arg "$org")")") || { echo ""; return; }
  errs=$(errors_of "$resp")
  if [[ -n $errs ]]; then
    [[ $errs == *"Cannot query field"* ]] && { echo ""; return; }
    die "refused: $errs"
  fi
  jq -r '.data.orgIndex.enterpriseId // empty' <<<"$resp"
}

# Every credential of every org read, and of its enterprise when it
# has one — a node's claim-time read joins both (gateway's
# credentialindex.go), so this reading must too, or an
# enterprise-scoped credential like `Temp - Subsciprtion` is never
# matched and reads "unknown" instead of what it really is.
credentials="[]"
for org in "${orgs[@]}"; do
  if [[ -n $fixture_dir ]]; then
    cred_resp=$(read_fixture "$fixture_dir/credentials-$org.json")
  else
    cred_resp=$(gql "$(printf '{ credentialIndexs(namespace: "runsheet", where: [{field: "org_id", op: EQ, value: %s}]) { id label status allowedNodeIds serves orgId enterpriseId } }' \
      "$(gql_arg "$org")")")
  fi
  credentials=$(jq -cn --argjson a "$credentials" --argjson b "$(jq -c '.data.credentialIndexs // []' <<<"$cred_resp")" '$a + $b')

  if [[ -z $fixture_dir ]]; then
    ent=$(org_enterprise "$org")
    if [[ -n $ent ]]; then
      ent_resp=$(gql "$(printf '{ credentialIndexs(namespace: "runsheet", where: [{field: "enterprise_id", op: EQ, value: %s}]) { id label status allowedNodeIds serves orgId enterpriseId } }' \
        "$(gql_arg "$ent")")")
      credentials=$(jq -cn --argjson a "$credentials" --argjson b "$(jq -c '.data.credentialIndexs // []' <<<"$ent_resp")" '$a + $b')
    fi
  fi
done
credentials=$(jq -c 'unique_by(.id)' <<<"$credentials")

# allowed: true when allowedNodeIds is empty, names one of the node's
# own ids, or — a legacy per-org node (ADR 005's amendment) — names
# the credential's own orgId.
allowed_credentials=$(jq -c --argjson nodeids "$node_ids" '
  map(
    . as $c
    | ($c.allowedNodeIds // []) as $a
    | ($a | length == 0) as $empty
    | ($a | any(. as $x | $nodeids | index($x) != null)) as $named
    | ($a | any(. == $c.orgId)) as $legacy
    | $c + {allowed: ($empty or $named or $legacy)}
  )
' <<<"$credentials")

reported=$(jq -c '
  ( [.[].holdingAccounts[]? | {label: .account, holding: .status}]
    + [.[].limits[]? | {label: .account, limitsStatus: .status}]
  )
  | group_by(.label)
  | map({
      label: .[0].label,
      holding: ([.[].holding] | map(select(. != null)) | first // "-"),
      limits: ([.[].limitsStatus] | map(select(. != null)) | first // "-")
    })
  | sort_by(.label)
' <<<"$node_rows")

echo "== reported"
if [[ "$(jq 'length' <<<"$reported")" -gt 0 ]]; then
  jq -r '.[] | "\(.label)\tholding=\(.holding)\tlimits=\(.limits)"' <<<"$reported"
else
  echo "(none)"
fi

echo "== platform"
if [[ "$(jq 'length' <<<"$allowed_credentials")" -gt 0 ]]; then
  jq -r 'sort_by(.label)[] | "\(.label)\tstatus=\(.status)\tallowed=\(if .allowed then "yes" else "no" end)\torg=\(.orgId)"' <<<"$allowed_credentials"
else
  echo "(none)"
fi

stale=$(jq -c --argjson creds "$allowed_credentials" '
  map(
    . as $r
    | ($creds | map(select(.label == $r.label))) as $matches
    | ($matches | map(select(.status == "active" and .allowed))) as $ok
    | if ($ok | length) > 0 then empty
      elif ($matches | length) == 0 then ($r + {reason: "unknown"})
      elif ($matches | any(.status == "deleted")) then ($r + {reason: "deleted"})
      elif ($matches | any(.status == "disabled")) then ($r + {reason: "disabled"})
      elif ($matches | any(.status == "active" and (.allowed | not))) then ($r + {reason: "not allowed"})
      else ($r + {reason: ($matches[0].status // "unknown")})
      end
  )
' <<<"$reported")

echo "== stale"
n=$(jq 'length' <<<"$stale")
if [[ "$n" -gt 0 ]]; then
  jq -r '.[] | "\(.label)\treason=\(.reason)"' <<<"$stale"
else
  echo "(none)"
fi
echo "stale: $n"

exit 0
