#!/usr/bin/env bash
# ensure-release.sh <tag> <sha> <release-dir>
#
# `gh release create` fails outright when the tag's release already
# exists ("release already exists"), which turned re-running the
# publish job after a mirror failure into deleting the release by hand
# (run 36499785802, 2026-09-29, "release exists"). This is the
# idempotent replacement release-tag.yml's "Create release on
# runsheet/mesh-cli" step and scripts/mirror-release.sh both call in
# place of a bare `gh release create`:
#
#   - no release for <tag> on GH_REPO yet: create it exactly as
#     before — the five files in <release-dir>, --prerelease when the
#     tag has a "-", TARGET's flag below.
#   - a release exists whose SHA256SUMS is byte-for-byte
#     <release-dir>/SHA256SUMS and whose assets include the three
#     binaries: kept, nothing created or touched on GitHub — a re-run
#     is a no-op there. With no TARGET (the source repo) the existing
#     release's SHA256SUMS.sigstore.json is downloaded over
#     <release-dir>'s freshly-signed one, so the mirror step that
#     follows publishes the exact bytes the source release already
#     carries — a re-run re-signs with a new Sigstore certificate every
#     time, and the existing bundle still verifies.
#   - a release exists whose SHA256SUMS differs, or that is missing one
#     of the three binaries: exits non-zero naming the asset and the
#     repo. Nothing is deleted or overwritten on GitHub either way — a
#     mismatch is for a person to resolve.
#
# Env (all required unless noted):
#   GH_REPO   the repo to check and, if needed, create on
#   GH_TOKEN  a token that can read and write it
#   NOTES     the release notes
#   TARGET    optional; set (e.g. "main") to pass --target <TARGET> to
#             `gh release create` — the mirror, whose tag does not
#             exist there yet, and whose GH_REPO is a different repo
#             than the one this workflow ran in, so the existing
#             release's Sigstore bundle is not reused. Left unset,
#             --verify-tag is passed instead — the source repo, whose
#             tag release-tag.yml already pushed.
#
# Needs gh and jq on PATH.
set -euo pipefail

tag=${1:?usage: ensure-release.sh <tag> <sha> <release-dir>}
sha=${2:?usage: ensure-release.sh <tag> <sha> <release-dir>}
release_dir=${3:?usage: ensure-release.sh <tag> <sha> <release-dir>}
: "${GH_REPO:?GH_REPO must be set}"
: "${GH_TOKEN:?GH_TOKEN must be set}"
: "${NOTES:?NOTES must name the release notes}"

cd "$(dirname "$0")/.."

binaries=(mesh-linux-amd64 mesh-linux-arm64 mesh-darwin-arm64)

flags=()
case "$tag" in *-*) flags+=(--prerelease) ;; esac
if [[ -n "${TARGET:-}" ]]; then
  flags+=(--target "$TARGET")
else
  flags+=(--verify-tag)
fi

assets_json=""
if assets_json=$(gh release view "$tag" --json assets 2>/dev/null); then
  for name in "${binaries[@]}" SHA256SUMS; do
    if ! jq -e --arg n "$name" '.assets[] | select(.name == $n)' <<<"$assets_json" >/dev/null; then
      echo "ensure-release: ${tag} on ${GH_REPO} is missing asset ${name}; not touching it — a person decides" >&2
      exit 1
    fi
  done

  existing_dir=$(mktemp -d)
  trap 'rm -rf "$existing_dir"' EXIT
  gh release download "$tag" -D "$existing_dir" -p SHA256SUMS -p SHA256SUMS.sigstore.json

  if ! cmp -s "$release_dir/SHA256SUMS" "$existing_dir/SHA256SUMS"; then
    echo "ensure-release: ${tag} on ${GH_REPO} has a SHA256SUMS that differs from ${release_dir}/SHA256SUMS; not touching it — a person decides" >&2
    exit 1
  fi

  echo "release ${tag} already on ${GH_REPO} with identical assets; keeping it"

  if [[ -z "${TARGET:-}" ]]; then
    cp "$existing_dir/SHA256SUMS.sigstore.json" "$release_dir/SHA256SUMS.sigstore.json"
  fi
  exit 0
fi

gh release create "$tag" \
  --title "$tag" \
  --notes "$NOTES" \
  "${flags[@]}" \
  "$release_dir/mesh-linux-amd64" "$release_dir/mesh-linux-arm64" "$release_dir/mesh-darwin-arm64" \
  "$release_dir/SHA256SUMS" "$release_dir/SHA256SUMS.sigstore.json"
