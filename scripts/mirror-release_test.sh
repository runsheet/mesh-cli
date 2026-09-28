#!/usr/bin/env bash
# Tests scripts/mirror-release.sh against a fake `gh` and `cosign` on PATH
# that record their argv, so the mirror's create, its check and the
# README push are asserted without reaching GitHub. Each fake logs one
# line per invocation, its arguments joined by \x1f (a byte that never
# appears in an argument itself), so an argument containing spaces —
# the notes — can still be matched as one field with a literal grep.
set -euo pipefail
cd "$(dirname "$0")/.."

fail() { echo "FAIL: $*" >&2; exit 1; }

# The line, if any, of $1 whose fields (a gh/cosign invocation) include
# every field named in the rest of the arguments.
call_with() {
  local file=$1; shift
  local line field
  while IFS= read -r line; do
    local ok=1
    for field in "$@"; do
      case $line in
        *$'\x1f'"$field"$'\x1f'* | "$field"$'\x1f'*) ;;
        *) ok=0 ;;
      esac
    done
    if [[ $ok == 1 ]]; then
      printf '%s' "$line"
      return 0
    fi
  done <"$file"
  return 1
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

bin="$work/bin"
mkdir -p "$bin"

release_dir="$work/release"
mkdir -p "$release_dir"
echo "binary" >"$release_dir/mesh-linux-amd64"
echo "binary" >"$release_dir/mesh-linux-arm64"
echo "binary" >"$release_dir/mesh-darwin-arm64"
echo "deadbeef  mesh-linux-amd64" >"$release_dir/SHA256SUMS"
echo '{"bundle":"fake"}' >"$release_dir/SHA256SUMS.sigstore.json"

log="$work/log"
: >"$log"

cat >"$bin/gh" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
{ printf 'gh'; for a in "$@"; do printf '\x1f%s' "$a"; done; printf '\n'; } >>"$FAKE_LOG"

case "${1:-}" in
  release)
    case "${2:-}" in
      create) exit 0 ;;
      download)
        dir=""
        prev=""
        for a in "$@"; do
          [[ $prev == "-D" ]] && dir=$a
          prev=$a
        done
        mkdir -p "$dir"
        cp "$FAKE_RELEASE_DIR/SHA256SUMS" "$dir/"
        cp "$FAKE_RELEASE_DIR/SHA256SUMS.sigstore.json" "$dir/"
        exit 0
        ;;
    esac
    ;;
  api)
    put=false
    jq_filter=""
    prev=""
    for a in "$@"; do
      [[ $a == "PUT" ]] && put=true
      [[ $prev == "--jq" ]] && jq_filter=$a
      prev=$a
    done
    if $put; then exit 0; fi
    case "$jq_filter" in
      .sha)
        [[ -n "${FAKE_README_SHA:-}" ]] || exit 1
        echo "$FAKE_README_SHA"
        ;;
      .content)
        echo "$FAKE_README_CONTENT"
        ;;
    esac
    ;;
esac
FAKE
chmod +x "$bin/gh"

cat >"$bin/cosign" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
{ printf 'cosign'; for a in "$@"; do printf '\x1f%s' "$a"; done; printf '\n'; } >>"$FAKE_LOG"
exit 0
FAKE
chmod +x "$bin/cosign"

export PATH="$bin:$PATH"
export FAKE_LOG="$log"
export FAKE_RELEASE_DIR="$release_dir"
export GH_TOKEN="fake-token"
export GH_REPO="runsheet/mesh"

# 1. A non-prerelease tag: the create names the five assets and the
#    given notes, via GH_REPO=runsheet/mesh; no --prerelease; the
#    README's content already matches, so no PUT.
notes="Release v0.2.187 built from runsheet/mesh-cli@abc123def. SHA256SUMS is signed keyless with Sigstore by runsheet/mesh-cli/.github/workflows/release-tag.yml; verify as in runsheet/mesh's README."
export NOTES="$notes"
export FAKE_README_SHA="old-readme-sha"
export FAKE_README_CONTENT=$(base64 -w0 dist/mirror/README.md)

: >"$log"
./scripts/mirror-release.sh v0.2.187 abc123def "$release_dir"

create_line=$(call_with "$log" "release" "create" "v0.2.187") || fail "no release create for v0.2.187"
[[ $create_line == *"$notes"* ]] || fail "the notes were not passed through unchanged"
[[ $create_line == *$'\x1f--prerelease'* ]] && fail "v0.2.187 must not be a prerelease"
for asset in mesh-linux-amd64 mesh-linux-arm64 mesh-darwin-arm64 SHA256SUMS SHA256SUMS.sigstore.json; do
  [[ $create_line == *"$asset"* ]] || fail "asset $asset missing from the create"
done
call_with "$log" "-X" "PUT" >/dev/null && fail "README pushed though its content already matches"
call_with "$log" "verify-blob" >/dev/null || fail "no cosign verify-blob against the mirror's bundle"
echo "ok: v0.2.187 — no prerelease flag, notes passed through, README unchanged"

# 2. A tag with a "-": --prerelease is added.
: >"$log"
./scripts/mirror-release.sh v0.2.188-rc.1 def456abc "$release_dir"
call_with "$log" "release" "create" "--prerelease" >/dev/null || fail "v0.2.188-rc.1 must be published as a prerelease"
echo "ok: v0.2.188-rc.1 — --prerelease"

# 3. The README's content differs: a PUT happens.
: >"$log"
export FAKE_README_CONTENT=$(printf 'a stale mirror README' | base64 -w0)
./scripts/mirror-release.sh v0.2.187 abc123def "$release_dir"
call_with "$log" "-X" "PUT" >/dev/null || fail "README not pushed though its content differs"
echo "ok: README pushed when its content differs"

echo "mirror-release_test.sh: PASS"
