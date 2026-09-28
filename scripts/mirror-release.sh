#!/usr/bin/env bash
# mirror-release.sh <tag> <sha> <release-dir>
#
# release-tag.yml's publish job calls this after it creates the signed
# release on runsheet/mesh-cli, to publish the identical release on the
# public mirror runsheet/mesh (docs/adr/adr-002-signed-tag-releases.md,
# "Amendment: the public mirror on runsheet/mesh"): same tag, same
# title, the same $NOTES the runsheet/mesh-cli release used, a
# prerelease only when the tag has a "-", and the same five assets.
# Then it proves the mirror is identical — the downloaded SHA256SUMS
# byte-for-byte the one just signed, and its Sigstore bundle still
# verifying against the runsheet/mesh-cli identity — and pushes
# dist/mirror/README.md to the mirror's README.md, only when its
# content differs from what is already there.
#
# <release-dir> holds the five files, named exactly as the release
# does: mesh-linux-amd64, mesh-linux-arm64, mesh-darwin-arm64,
# SHA256SUMS, SHA256SUMS.sigstore.json.
#
# Env (all required):
#   GH_TOKEN   a token that can write the mirror (RELEASE_TOKEN): a
#              fine-grained token scoped to it with Contents: read and
#              write, or the runsheet org App's installation token
#   GH_REPO    the mirror, runsheet/mesh
#   NOTES      the release notes — computed once by release-tag.yml and
#              shared with its own runsheet/mesh-cli release, so both
#              read the same text
#
# Needs gh and cosign on PATH. Any failure here fails the workflow: a
# release that exists only on runsheet/mesh-cli is not this contract.
set -euo pipefail

tag=${1:?usage: mirror-release.sh <tag> <sha> <release-dir>}
sha=${2:?usage: mirror-release.sh <tag> <sha> <release-dir>}
release_dir=${3:?usage: mirror-release.sh <tag> <sha> <release-dir>}
: "${GH_REPO:?GH_REPO must name the mirror, e.g. runsheet/mesh}"
: "${GH_TOKEN:?GH_TOKEN must be a token that can write ${GH_REPO}}"
: "${NOTES:?NOTES must name the release notes}"

cd "$(dirname "$0")/.."

source_identity="runsheet/mesh-cli"
readme=dist/mirror/README.md

flags=()
case "$tag" in *-*) flags+=(--prerelease) ;; esac

gh release create "$tag" \
  --title "$tag" \
  --notes "$NOTES" \
  "${flags[@]}" \
  "$release_dir/mesh-linux-amd64" "$release_dir/mesh-linux-arm64" "$release_dir/mesh-darwin-arm64" \
  "$release_dir/SHA256SUMS" "$release_dir/SHA256SUMS.sigstore.json"

mirror_dir=$(mktemp -d)
trap 'rm -rf "$mirror_dir"' EXIT

gh release download "$tag" -D "$mirror_dir" \
  -p SHA256SUMS -p SHA256SUMS.sigstore.json

cmp "$release_dir/SHA256SUMS" "$mirror_dir/SHA256SUMS"

cosign verify-blob \
  --bundle "$mirror_dir/SHA256SUMS.sigstore.json" \
  --certificate-identity "https://github.com/${source_identity}/.github/workflows/release-tag.yml@refs/tags/${tag}" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "$mirror_dir/SHA256SUMS"

# The README, pushed only when its content differs from what is there.
current_sha=""
current_content=""
if current_sha=$(gh api "repos/${GH_REPO}/contents/README.md" --jq '.sha' 2>/dev/null); then
  current_content=$(gh api "repos/${GH_REPO}/contents/README.md" --jq '.content' | tr -d '\n' | base64 -d)
fi
new_content=$(cat "$readme")

if [[ $current_content != "$new_content" ]]; then
  encoded=$(base64 -w0 "$readme")
  args=(-X PUT "repos/${GH_REPO}/contents/README.md" \
    -f "message=mirror: update README from ${source_identity}@${sha}" \
    -f "content=${encoded}")
  if [[ -n $current_sha ]]; then
    args+=(-f "sha=${current_sha}")
  fi
  gh api "${args[@]}" >/dev/null
fi
