#!/usr/bin/env bash
# Takes the reading item 6 of release-tag-mirrors-with-a-rigger-binding
# owes: which token path release-tag.yml's "Mirror the release to
# runsheet/mesh" step took on each of the last `count` completed v* runs
# — exchanged through Rigger, the RELEASE_TOKEN fallback, or neither —
# on the pattern of scripts/node-reload-reading.sh. Before this, nobody
# could answer "has a v* run mirrored on the exchanged token yet?"
# without reading run logs by hand (item 4 failed three times over it).
# Item 12 adds whether the RELEASE_TOKEN secret itself still exists on
# the repo (`gh secret list`) — the goal's acceptance says no such
# secret exists, and until this nobody could read that either.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: scripts/mirror-token-reading.sh [count] [--help]

Lists the last <count> (default 5) completed runs of release-tag.yml on
runsheet/mesh-cli (`gh run list --repo runsheet/mesh-cli --workflow
release-tag.yml --status completed --json
databaseId,headBranch,createdAt,conclusion`), fetches each run's log
(`gh run view <id> --repo runsheet/mesh-cli --log`) and reads the
"Mirror the release to runsheet/mesh" step's own `mirror token: ` line
— never an echoed source line, which carries ANSI colour and prints
`echo "mirror token:` — to say which path the step took:

  exchanged  the run's OIDC identity was exchanged through Rigger
  fallback   the exchange failed and RELEASE_TOKEN was used instead
  none       the exchange failed, RELEASE_TOKEN was not set, and the
             step (and the workflow) failed
  absent     the job never reached the step — an earlier step failed

Prints one line per run: <tag>\t<run id>\t<created>\t<path>\t<reason>,
where <reason> is the gateway's refusal for fallback and none, "failed
at <step>" for absent, and empty for exchanged. A line,
`latest: <path> (<tag>, run <id>)`, names the newest run that reached
the step, followed by a final line reading whether the RELEASE_TOKEN
secret itself still exists on the repo (`gh secret list --repo
<repo>`):

  secret: present (RELEASE_TOKEN, updated <date>)
  secret: absent (RELEASE_TOKEN)
  secret: unknown (<reason>)   gh could not list secrets (no admin
                               scope, 403, …) — <reason> is gh's own
                               first line of stderr

Always exits 0: the reading is the output, never a verdict.

Needs gh and jq on PATH, authenticated against runsheet/mesh-cli.
EOF
}

count=5
case "${1:-}" in
  --help | -h) usage; exit 0 ;;
  "") ;;
  *[!0-9]*) echo "mirror-token-reading: count wants a whole number, got '$1'" >&2; exit 2 ;;
  *) count=$1 ;;
esac

repo=${MIRROR_TOKEN_READING_REPO:-runsheet/mesh-cli}

runs_json=$(gh run list --repo "$repo" --workflow release-tag.yml --status completed \
  --json databaseId,headBranch,createdAt,conclusion --limit "$count")

latest_path=""
latest_tag=""
latest_id=""

while IFS=$'\t' read -r id tag created; do
  [ -n "$id" ] || continue
  log=$(gh run view "$id" --repo "$repo" --log)

  line=""
  errstep=""
  while IFS=$'\t' read -r job step rest; do
    [ -n "${job:-}" ] || continue
    text="${rest#*Z }"
    if [[ -z $line && $text == "mirror token: "* ]]; then
      line=$text
    fi
    if [[ -z $errstep && $text == *"##[error]Process completed with exit code"* ]]; then
      errstep=$step
    fi
  done <<<"$log"

  reason=""
  if [[ -z $line ]]; then
    path="absent"
    if [[ -n $errstep ]]; then
      reason="failed at $errstep"
    else
      reason="the job never reached the step"
    fi
  elif [[ $line == "mirror token: exchanged through rigger"* ]]; then
    path="exchanged"
  elif [[ $line == "mirror token: RELEASE_TOKEN fallback ("* ]]; then
    path="fallback"
    reason=${line%)}
    reason=${reason#"mirror token: RELEASE_TOKEN fallback (exchange failed: "}
  elif [[ $line == "mirror token: none"* ]]; then
    path="none"
    reason=${line%" and RELEASE_TOKEN is not set"}
    reason=${reason%)}
    reason=${reason#"mirror token: none — the exchange through rigger failed ("}
  else
    path="absent"
    reason="unrecognised mirror token line: $line"
  fi

  printf '%s\t%s\t%s\t%s\t%s\n' "$tag" "$id" "$created" "$path" "$reason"

  if [[ -z $latest_path && $path != "absent" ]]; then
    latest_path=$path
    latest_tag=$tag
    latest_id=$id
  fi
done < <(jq -r '.[] | [.databaseId, .headBranch, .createdAt] | @tsv' <<<"$runs_json")

if [[ -n $latest_path ]]; then
  echo "latest: $latest_path ($latest_tag, run $latest_id)"
else
  echo "latest: none of the last $count run(s) reached the step"
fi

secret_err=$(mktemp)
trap 'rm -f "$secret_err"' EXIT

if secret_json=$(gh secret list --repo "$repo" --json name,updatedAt 2>"$secret_err"); then
  updated=$(jq -r '.[] | select(.name == "RELEASE_TOKEN") | .updatedAt' <<<"$secret_json")
  if [[ -n $updated ]]; then
    echo "secret: present (RELEASE_TOKEN, updated $updated)"
  else
    echo "secret: absent (RELEASE_TOKEN)"
  fi
else
  reason=$(head -n1 "$secret_err")
  echo "secret: unknown ($reason)"
fi

exit 0
