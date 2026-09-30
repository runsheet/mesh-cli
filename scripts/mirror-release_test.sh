#!/usr/bin/env bash
# Tests scripts/mirror-release.sh against a fake `gh` and `cosign` on PATH
# that record their argv, so the mirror's create, its check and the
# README and binding pushes are asserted without reaching GitHub. Each fake logs one
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
    # The mirror's tree is $FAKE_REMOTE: repos/<repo>/contents/<path>
    # answers from $FAKE_REMOTE/<path>, absent when there is no file.
    put=false
    jq_filter=""
    path=""
    prev=""
    for a in "$@"; do
      [[ $a == "PUT" ]] && put=true
      [[ $prev == "--jq" ]] && jq_filter=$a
      [[ $a == repos/*/contents/* ]] && path=${a#repos/*/*/contents/}
      prev=$a
    done
    if $put; then exit 0; fi
    [[ -f "$FAKE_REMOTE/$path" ]] || exit 1
    case "$jq_filter" in
      .sha) echo "sha-of-$path" ;;
      .content) base64 -w0 "$FAKE_REMOTE/$path"; echo ;;
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
export FAKE_REMOTE="$work/remote"

binding=.runsheet/bindings/mesh-cli-release.yaml
readme_put=(-X PUT repos/runsheet/mesh/contents/README.md)
binding_put=(-X PUT "repos/runsheet/mesh/contents/$binding")

# The mirror as it should be: README.md and the binding already there.
mirror_up_to_date() {
  rm -rf "$FAKE_REMOTE"
  mkdir -p "$FAKE_REMOTE/.runsheet/bindings"
  cp dist/mirror/README.md "$FAKE_REMOTE/README.md"
  cp "dist/mirror/$binding" "$FAKE_REMOTE/$binding"
}

# 1. A non-prerelease tag: the create names the five assets and the
#    given notes, via GH_REPO=runsheet/mesh; no --prerelease; the
#    README's content already matches, so no PUT.
notes="Release v0.2.187 built from runsheet/mesh-cli@abc123def. SHA256SUMS is signed keyless with Sigstore by runsheet/mesh-cli/.github/workflows/release-tag.yml; verify as in runsheet/mesh's README."
export NOTES="$notes"
mirror_up_to_date

: >"$log"
./scripts/mirror-release.sh v0.2.187 abc123def "$release_dir"

create_line=$(call_with "$log" "release" "create" "v0.2.187") || fail "no release create for v0.2.187"
[[ $create_line == *"$notes"* ]] || fail "the notes were not passed through unchanged"
[[ $create_line == *$'\x1f--prerelease'* ]] && fail "v0.2.187 must not be a prerelease"
for asset in mesh-linux-amd64 mesh-linux-arm64 mesh-darwin-arm64 SHA256SUMS SHA256SUMS.sigstore.json; do
  [[ $create_line == *"$asset"* ]] || fail "asset $asset missing from the create"
done
call_with "$log" "${readme_put[@]}" >/dev/null && fail "README pushed though its content already matches"
call_with "$log" "${binding_put[@]}" >/dev/null && fail "binding pushed though it is identical"
call_with "$log" "verify-blob" >/dev/null || fail "no cosign verify-blob against the mirror's bundle"
echo "ok: v0.2.187 — no prerelease flag, notes passed through, README and binding unchanged"

# 2. A tag with a "-": --prerelease is added.
: >"$log"
./scripts/mirror-release.sh v0.2.188-rc.1 def456abc "$release_dir"
call_with "$log" "release" "create" "--prerelease" >/dev/null || fail "v0.2.188-rc.1 must be published as a prerelease"
echo "ok: v0.2.188-rc.1 — --prerelease"

# 3. The README's content differs: a PUT happens.
: >"$log"
mirror_up_to_date
printf 'a stale mirror README' >"$FAKE_REMOTE/README.md"
./scripts/mirror-release.sh v0.2.187 abc123def "$release_dir"
readme_line=$(call_with "$log" "${readme_put[@]}") || fail "README not pushed though its content differs"
[[ $readme_line == *$'\x1f'"sha=sha-of-README.md"* ]] || fail "README PUT does not name the sha it replaces"
call_with "$log" "${binding_put[@]}" >/dev/null && fail "binding pushed though it is identical"
echo "ok: README pushed when its content differs"

# 4. The binding is absent on the mirror: it is PUT, with its content
#    and no sha; the README, identical, is not.
: >"$log"
mirror_up_to_date
rm "$FAKE_REMOTE/$binding"
./scripts/mirror-release.sh v0.2.187 abc123def "$release_dir"
binding_line=$(call_with "$log" "${binding_put[@]}") || fail "binding not PUT though absent on the mirror"
[[ $binding_line == *$'\x1f'"content=$(base64 -w0 "dist/mirror/$binding")"* ]] || fail "binding PUT without its content"
[[ $binding_line == *$'\x1f'sha=* ]] && fail "binding PUT names a sha though the file is new"
call_with "$log" "${readme_put[@]}" >/dev/null && fail "README pushed though its content already matches"
echo "ok: $binding PUT to the mirror when absent"

# 5. The binding differs on the mirror: it is PUT over the old one.
: >"$log"
mirror_up_to_date
printf 'name: something-else\n' >"$FAKE_REMOTE/$binding"
./scripts/mirror-release.sh v0.2.187 abc123def "$release_dir"
binding_line=$(call_with "$log" "${binding_put[@]}") || fail "binding not PUT though it differs"
[[ $binding_line == *$'\x1f'"sha=sha-of-$binding"* ]] || fail "binding PUT does not name the sha it replaces"
echo "ok: $binding PUT when it differs, not when identical"

echo "mirror-release_test.sh: PASS"
