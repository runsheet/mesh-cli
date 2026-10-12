#!/usr/bin/env bash
# Tests scripts/artifact-registry-setup.sh against a fake `gcloud` on
# PATH (the pattern of mirror-token_test.sh): the fake records each
# call's argv, one argument per line, and the policy file it was given.
set -euo pipefail
cd "$(dirname "$0")/.."

fail() { echo "FAIL: $*" >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
bin="$work/bin"
mkdir -p "$bin"

cat >"$bin/gcloud" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
n=$(find "$FAKE_CALLS" -name 'call-*' | wc -l)
printf '%s\n' "$@" >"$FAKE_CALLS/call-$n"
for a in "$@"; do
  case $a in --policy=*) cp "${a#--policy=}" "$FAKE_CALLS/policy-$n.json" ;; esac
done
FAKE
chmod +x "$bin/gcloud"

export PATH="$bin:$PATH"
export FAKE_CALLS="$work/calls"
mkdir -p "$FAKE_CALLS"

has() { grep -qxF -- "$2" "$1" || fail "$(basename "$1") has no argument '$2': $(tr '\n' ' ' <"$1")"; }

# The run: two gcloud calls, the binding then the cleanup policies.
scripts/artifact-registry-setup.sh >"$work/out" 2>"$work/err" || fail "exited non-zero: $(cat "$work/err")"
calls=$(find "$FAKE_CALLS" -name 'call-*' | wc -l)
[[ $calls -eq 2 ]] || fail "gcloud called $calls times, want 2"

c="$FAKE_CALLS/call-0"
[[ $(head -n4 "$c" | tr '\n' ' ') == "artifacts repositories add-iam-policy-binding runsheet " ]] ||
  fail "first call is not add-iam-policy-binding runsheet: $(tr '\n' ' ' <"$c")"
has "$c" --member=allUsers
has "$c" --role=roles/artifactregistry.reader
has "$c" --location=us-central1
has "$c" --project=runsheet-prod
echo "ok: add-iam-policy-binding allUsers roles/artifactregistry.reader on runsheet, us-central1, runsheet-prod"

c="$FAKE_CALLS/call-1"
[[ $(head -n4 "$c" | tr '\n' ' ') == "artifacts repositories set-cleanup-policies runsheet " ]] ||
  fail "second call is not set-cleanup-policies runsheet: $(tr '\n' ' ' <"$c")"
has "$c" --location=us-central1
has "$c" --project=runsheet-prod
has "$c" --no-dry-run
grep -q '^--policy=' "$c" || fail "set-cleanup-policies has no --policy"
p="$FAKE_CALLS/policy-1.json"
[[ -s $p ]] || fail "the policy file was not readable"
jq -e 'type == "array" and length > 0' "$p" >/dev/null || fail "policy is not a non-empty array"
jq -e 'all(.[]; ((.condition // .mostRecentVersions).packageNamePrefixes) == ["mesh-node"])' "$p" >/dev/null ||
  fail "a policy is not scoped to packageNamePrefixes [mesh-node]"
jq -e 'any(.[]; .action.type == "Keep" and .mostRecentVersions.keepCount == 10)' "$p" >/dev/null ||
  fail "no Keep of the 10 most recent versions"
jq -e 'any(.[]; .action.type == "Keep" and .condition.tagState == "tagged" and .condition.newerThan == "30d")' "$p" >/dev/null ||
  fail "no Keep of versions tagged in the last 30 days"
jq -e 'any(.[]; .action.type == "Delete")' "$p" >/dev/null || fail "no Delete of the rest"
echo "ok: set-cleanup-policies, every policy scoped to mesh-node, keepCount 10, tagged 30d kept, the rest deleted"

# --dry-run: both commands printed, gcloud never run.
rm -f "$FAKE_CALLS"/*
scripts/artifact-registry-setup.sh --dry-run >"$work/out" 2>"$work/err" || fail "--dry-run exited non-zero: $(cat "$work/err")"
calls=$(find "$FAKE_CALLS" -name 'call-*' | wc -l)
[[ $calls -eq 0 ]] || fail "--dry-run ran gcloud $calls times"
[[ $(wc -l <"$work/out") -eq 2 ]] || fail "--dry-run printed $(wc -l <"$work/out") lines, want 2: $(cat "$work/out")"
grep -q '^gcloud artifacts repositories add-iam-policy-binding runsheet --project=runsheet-prod --location=us-central1 --member=allUsers --role=roles/artifactregistry.reader$' "$work/out" ||
  fail "--dry-run did not print the binding: $(cat "$work/out")"
grep -q '^gcloud artifacts repositories set-cleanup-policies runsheet --project=runsheet-prod --location=us-central1 --policy=.*/scripts/artifact-registry-cleanup.json --no-dry-run$' "$work/out" ||
  fail "--dry-run did not print the cleanup policies: $(cat "$work/out")"
echo "ok: --dry-run prints both commands and runs no gcloud"

# An unknown argument is refused before anything runs.
set +e
scripts/artifact-registry-setup.sh --bogus >"$work/out" 2>"$work/err"
code=$?
set -e
[[ $code -eq 2 ]] || fail "unknown argument exited $code, want 2"
[[ -z $(find "$FAKE_CALLS" -name 'call-*') ]] || fail "unknown argument ran gcloud"
echo "ok: an unknown argument is refused"

echo "artifact-registry-setup_test.sh: PASS"
