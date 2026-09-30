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
# verifying against the runsheet/mesh-cli identity — and publishes
# what dist/mirror/ holds for the mirror's tree (publish_file), each
# only when its content differs from what is already there:
#   dist/mirror/README.md                                 → README.md
#   dist/mirror/.runsheet/bindings/mesh-cli-release.yaml  → the Rigger
#     binding the mirror carries, against which the runsheet gateway
#     exchanges this workflow's OIDC token for the GH_TOKEN below
#     (docs/adr/adr-015-workflow-tokens-exchanged-through-rigger.md).
#
# <release-dir> holds the five files, named exactly as the release
# does: mesh-linux-amd64, mesh-linux-arm64, mesh-darwin-arm64,
# SHA256SUMS, SHA256SUMS.sigstore.json.
#
# Env (all required):
#   GH_TOKEN   a token that can write the mirror's contents: the one
#              scripts/exchange-workflow-token.sh exchanged through
#              Rigger (one repository, contents: write, an hour), or —
#              transitional, until the runsheet half lands — the
#              RELEASE_TOKEN secret release-tag.yml falls back to
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

flags=()
case "$tag" in *-*) flags+=(--prerelease) ;; esac

# --target: the mirror has no commit of ours, so the release's tag is
# created there at its default branch's head; without it gh refuses,
# "tag exists locally but has not been pushed" (v0.2.187, run 36494313388).
gh release create "$tag" \
  --target main \
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

# publish_file <local> <remote-path>: puts <local> at <remote-path> on
# the mirror's default branch, only when its bytes differ from what is
# already there (absent counts as different). The remote file is decoded
# into a file and compared with cmp, never through $(…), which strips
# trailing newlines: a file that differs only by its last newline is
# still a change.
publish_file() {
  local local_file=$1 remote=$2
  local current_sha="" current_file encoded
  current_file="$mirror_dir/publish-current"
  rm -f "$current_file"
  if current_sha=$(gh api "repos/${GH_REPO}/contents/${remote}" --jq '.sha' 2>/dev/null); then
    gh api "repos/${GH_REPO}/contents/${remote}" --jq '.content' | tr -d '\n' | base64 -d >"$current_file"
  else
    current_sha=""
  fi

  if [[ -n $current_sha ]] && cmp -s "$local_file" "$current_file"; then
    return 0
  fi
  encoded=$(base64 -w0 "$local_file")
  local args=(-X PUT "repos/${GH_REPO}/contents/${remote}"
    -f "message=mirror: update ${remote} from ${source_identity}@${sha}"
    -f "content=${encoded}")
  if [[ -n $current_sha ]]; then
    args+=(-f "sha=${current_sha}")
  fi
  gh api "${args[@]}" >/dev/null
}

publish_file dist/mirror/README.md README.md
publish_file dist/mirror/.runsheet/bindings/mesh-cli-release.yaml .runsheet/bindings/mesh-cli-release.yaml
