#!/usr/bin/env bash
# Takes the reading item 6 of the-nodes-attestation-reaches-the-platform:
# across the latest pull-request runs of runsheet/runsheet's test
# workflow, how many `verify` jobs answer `verdict: verified` and how
# many of those skip the three e2e shard jobs (ADR 032, decision 2's "As
# built"). Before this, the number lived in a person reading Actions by
# hand; the goal's own reading of 2026-10-03 (0 of the last 24) is what
# this script was written to confirm and keep taking.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: scripts/attestation-reading.sh [repo] [workflow] [count] [--help]

Reads the last <count> (default 24) completed pull-request runs of
<workflow> (default test.yml) in <repo> (default runsheet/runsheet)
with `gh run list`, and for each run reads its `verify` job's verdict
and whether the three e2e shard jobs (`shard (e2e 1/3)`, `2/3`, `3/3`,
ADR 032 decision 2) concluded skipped, printing one line per run:

  <run url> <head sha> verdict=<verdict> shards_skipped=<yes|no>

then two summary lines:

  verified: X/N
  shards skipped on a verified verdict: Y/N

X is the runs whose verify job answered verdict verified; Y is the
runs that were both verified and skipped the shards; N is the total
runs read. Nothing here is a pass or fail condition (ADR 032's honesty
check): the reading is whatever it is.

The verdict is read from the verify job's step outputs when `gh run
view --json jobs` lists them (it does not today — GitHub's API does
not expose step outputs after the fact), else from the job's own log
(`gh run view --log`), by the first `verdict: <word>` (or `verdict=
<word>`) line in it. A run with no `verify` job reads verdict
`missing`; one whose log names no verdict reads `unknown`.

It then reads the scorecard's CI-minutes-saved figure (ADR 032, the
honesty check) for the repo's org through the gateway
(https://api.runsheet.dev/graphql), on an rs_* console token in
~/.mesh/token, matching the org by the repo's owner and the project by
the repo's name, and prints one of:

  ciMinutesSaved: <n>
  scorecard: unavailable (<why>)

Needs gh (authenticated for <repo>) and jq on PATH; curl too for the
scorecard read.
EOF
}

repo=runsheet/runsheet
workflow=test.yml
count=24

positional=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --help | -h) usage; exit 0 ;;
    --) shift; positional+=("$@"); break ;;
    -*) echo "attestation-reading: unknown option: $1" >&2; usage >&2; exit 2 ;;
    *) positional+=("$1"); shift ;;
  esac
done

[[ ${#positional[@]} -ge 1 ]] && repo=${positional[0]}
[[ ${#positional[@]} -ge 2 ]] && workflow=${positional[1]}
[[ ${#positional[@]} -ge 3 ]] && count=${positional[2]}

die() {
  echo "attestation-reading: $*" >&2
  exit 1
}

command -v gh >/dev/null 2>&1 || die "gh is not on PATH"
command -v jq >/dev/null 2>&1 || die "jq is not on PATH"

shard_names_json='["shard (e2e 1/3)","shard (e2e 2/3)","shard (e2e 3/3)"]'

# verdict_of <jobs-json> <run-id>: the verify job's verdict, read from
# its own log (gh exposes no step outputs after the fact).
verdict_of() {
  local jobs_json=$1 run_id=$2
  local verify_job_id
  verify_job_id=$(jq -r '[.jobs[] | select(.name == "verify")][0].databaseId // empty' <<<"$jobs_json")
  if [[ -z $verify_job_id ]]; then
    echo missing
    return
  fi
  local log
  log=$(gh run view "$run_id" --repo "$repo" --job "$verify_job_id" --log 2>/dev/null || true)
  local v
  v=$(grep -ioE 'verdict[: =]+[a-z_]+' <<<"$log" | head -n1 | grep -oE '[a-z_]+$' || true)
  if [[ -z $v ]]; then
    echo unknown
  else
    tr '[:upper:]' '[:lower:]' <<<"$v"
  fi
}

# shards_skipped_of <jobs-json>: yes when all three e2e shard jobs
# concluded skipped, else no.
shards_skipped_of() {
  local jobs_json=$1
  local n
  n=$(jq --argjson names "$shard_names_json" '
    [.jobs[]? | select(.name as $n | $names | index($n)) | select(.conclusion == "skipped")] | length
  ' <<<"$jobs_json")
  if [[ $n -ge 3 ]]; then echo yes; else echo no; fi
}

runs_json=$(gh run list --repo "$repo" --workflow "$workflow" --event pull_request --status completed --limit "$count" \
  --json databaseId,headSha,url,conclusion) || die "gh run list failed"

total=$(jq 'length' <<<"$runs_json")
verified=0
skipped_on_verified=0

while IFS= read -r row; do
  [[ -n $row ]] || continue
  run_id=$(jq -r '.databaseId' <<<"$row")
  head_sha=$(jq -r '.headSha' <<<"$row")
  url=$(jq -r '.url' <<<"$row")

  jobs_json=$(gh run view "$run_id" --repo "$repo" --json jobs) || die "gh run view $run_id failed"

  verdict=$(verdict_of "$jobs_json" "$run_id")
  shards_skipped=$(shards_skipped_of "$jobs_json")

  if [[ $verdict == verified ]]; then
    verified=$((verified + 1))
    [[ $shards_skipped == yes ]] && skipped_on_verified=$((skipped_on_verified + 1))
  fi

  echo "$url $head_sha verdict=$verdict shards_skipped=$shards_skipped"
done < <(jq -c '.[]' <<<"$runs_json")

echo "verified: $verified/$total"
echo "shards skipped on a verified verdict: $skipped_on_verified/$total"

# post <query>: the raw gateway response to one GraphQL query.
post() {
  local token=$1 query=$2 body
  body=$(jq -cn --arg q "$query" '{query: $q}')
  curl -sS -m 15 -X POST \
    -H "Authorization: Bearer $token" \
    -H "Content-Type: application/json" \
    --data-binary "$body" \
    https://api.runsheet.dev/graphql
}

errors_of() {
  jq -r '[.errors[]?.message] | join("; ")' <<<"$1" 2>/dev/null || true
}

scorecard_line() {
  local token_file=${HOME:-}/.mesh/token
  if [[ ! -f $token_file ]]; then
    echo "scorecard: unavailable (no ~/.mesh/token)"
    return
  fi
  local token
  token=$(head -n1 "$token_file")
  if [[ $token != rs_* ]]; then
    echo "scorecard: unavailable (no rs_* token in ~/.mesh/token)"
    return
  fi
  if ! command -v curl >/dev/null 2>&1; then
    echo "scorecard: unavailable (curl is not on PATH)"
    return
  fi

  local org_login=${repo%%/*}
  local repo_name=${repo#*/}

  local orgs_resp
  orgs_resp=$(post "$token" '{ myOrgs }') || { echo "scorecard: unavailable (the gateway did not answer)"; return; }
  jq -e . >/dev/null 2>&1 <<<"$orgs_resp" || { echo "scorecard: unavailable (the gateway answered with no JSON)"; return; }
  local err
  err=$(errors_of "$orgs_resp")
  [[ -z $err ]] || { echo "scorecard: unavailable (refused: $err)"; return; }

  local org_id
  org_id=$(jq -r --arg l "$org_login" '
    .data.myOrgs[]? | select(((.tenantSlug // "") == $l) or ((.login // "") == $l)) | .id
  ' <<<"$orgs_resp" | head -n1)
  [[ -n $org_id ]] || { echo "scorecard: unavailable (no org named '$org_login' in myOrgs)"; return; }

  local sc_resp
  sc_resp=$(post "$token" "{ scorecard(orgId: \"$org_id\") }") || { echo "scorecard: unavailable (the gateway did not answer)"; return; }
  jq -e . >/dev/null 2>&1 <<<"$sc_resp" || { echo "scorecard: unavailable (the gateway answered with no JSON)"; return; }
  err=$(errors_of "$sc_resp")
  [[ -z $err ]] || { echo "scorecard: unavailable (refused: $err)"; return; }

  local n
  n=$(jq -r --arg name "$repo_name" '
    ([.data.scorecard.projects[]? | select((.projectName // "" | ascii_downcase) == ($name | ascii_downcase))][0]) as $p
    | if $p == null then empty
      else (($p.windows // []) | sort_by(-.days) | .[0].attestations.ciMinutesSaved // empty)
      end
  ' <<<"$sc_resp")
  if [[ -z $n ]]; then
    echo "scorecard: unavailable (no project named '$repo_name' with an attestations figure)"
  else
    echo "ciMinutesSaved: $n"
  fi
}

scorecard_line
