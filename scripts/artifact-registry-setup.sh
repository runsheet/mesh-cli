#!/usr/bin/env bash
# artifact-registry-setup.sh [--dry-run]
#
# Makes the node image's Artifact Registry repository what the goal
# the-node-image-lives-on-artifact-registry asks of it: the Docker
# repository `runsheet` in runsheet-prod, us-central1, where
# release-tag.yml pushes `mesh-node:<version>`.
#
# - anyone may read it: the allUsers roles/artifactregistry.reader
#   binding on the repository, so a node with no registry login pulls
#   its sandbox image;
# - its mesh-node images are cleaned up by
#   scripts/artifact-registry-cleanup.json, every policy scoped to the
#   package prefix mesh-node (the platform's own images in the same
#   repository are untouched): keep the newest ten versions, keep
#   anything tagged in the last 30 days, delete the rest. Keep policies
#   win over the delete.
#
# Both commands are idempotent: a binding already present is left as it
# is, and the cleanup policies are replaced with the file's whole.
# --dry-run prints each gcloud command and runs none.
#
# The workload identity the release workflow pushes under is set up
# separately.
set -euo pipefail

project=runsheet-prod
location=us-central1
repository=runsheet
here=$(cd "$(dirname "$0")" && pwd)
policy="$here/artifact-registry-cleanup.json"

dry_run=false
for arg in "$@"; do
  case $arg in
    --dry-run) dry_run=true ;;
    -h | --help) sed -n '2,/^set -euo/{/^#/s/^# \{0,1\}//p}' "$0"; exit 0 ;;
    *) echo "artifact-registry-setup: unknown argument $arg" >&2; exit 2 ;;
  esac
done

[[ -r $policy ]] || { echo "artifact-registry-setup: no policy file at $policy" >&2; exit 1; }

run() {
  if $dry_run; then
    printf '%q ' "$@" | sed 's/ $//'
    echo
  else
    echo "+ $*" >&2
    "$@"
  fi
}

run gcloud artifacts repositories add-iam-policy-binding "$repository" \
  --project="$project" --location="$location" \
  --member=allUsers --role=roles/artifactregistry.reader

run gcloud artifacts repositories set-cleanup-policies "$repository" \
  --project="$project" --location="$location" \
  --policy="$policy" --no-dry-run
