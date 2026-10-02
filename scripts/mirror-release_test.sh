#!/usr/bin/env bash
# Tests scripts/mirror-release.sh and scripts/ensure-release.sh against a
# fake `gh` and `cosign` on PATH that record their argv, so the mirror's
# create, its check, the README and binding pushes, and ensure-release.sh's
# idempotent create-or-keep are asserted without reaching GitHub. Each fake logs one
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
reset_release_dir() {
  rm -rf "$release_dir"
  mkdir -p "$release_dir"
  echo "binary" >"$release_dir/mesh-linux-amd64"
  echo "binary" >"$release_dir/mesh-linux-arm64"
  echo "binary" >"$release_dir/mesh-darwin-arm64"
  echo "deadbeef  mesh-linux-amd64" >"$release_dir/SHA256SUMS"
  echo '{"bundle":"freshly-signed"}' >"$release_dir/SHA256SUMS.sigstore.json"
}
reset_release_dir

# A directory that looks like an existing GitHub release whose assets
# are identical to $release_dir's, but whose Sigstore bundle is a
# different (distinguishable) blob — as a real re-signed one would be.
existing_identical_dir="$work/existing-identical"
mkdir -p "$existing_identical_dir"
cp "$release_dir/SHA256SUMS" "$existing_identical_dir/"
echo '{"bundle":"already-on-github"}' >"$existing_identical_dir/SHA256SUMS.sigstore.json"

# A directory whose SHA256SUMS differs from $release_dir's.
existing_differing_dir="$work/existing-differing"
mkdir -p "$existing_differing_dir"
echo "cafebabe  mesh-linux-amd64" >"$existing_differing_dir/SHA256SUMS"
echo '{"bundle":"already-on-github"}' >"$existing_differing_dir/SHA256SUMS.sigstore.json"

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
      view)
        if [[ "${FAKE_RELEASE_EXISTS:-0}" == "1" ]]; then
          names="${FAKE_RELEASE_ASSETS:-mesh-linux-amd64 mesh-linux-arm64 mesh-darwin-arm64 SHA256SUMS SHA256SUMS.sigstore.json}"
          out='{"assets":['
          first=1
          for n in $names; do
            [[ $first == 1 ]] || out+=','
            first=0
            out+="{\"name\":\"$n\"}"
          done
          out+=']}'
          printf '%s' "$out"
          exit 0
        else
          exit 1
        fi
        ;;
      download)
        dir=""
        prev=""
        for a in "$@"; do
          [[ $prev == "-D" ]] && dir=$a
          prev=$a
        done
        mkdir -p "$dir"
        src="${FAKE_DOWNLOAD_DIR:-$FAKE_RELEASE_DIR}"
        [[ -f "$src/SHA256SUMS" ]] && cp "$src/SHA256SUMS" "$dir/"
        [[ -f "$src/SHA256SUMS.sigstore.json" ]] && cp "$src/SHA256SUMS.sigstore.json" "$dir/"
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
unset FAKE_DOWNLOAD_DIR FAKE_RELEASE_EXISTS FAKE_RELEASE_ASSETS || true

# 1. A non-prerelease tag, no release on the mirror yet: the create
#    names the five assets and the given notes, via GH_REPO=runsheet/mesh;
#    no --prerelease; the README's content already matches, so no PUT.
notes="Release v0.2.187 built from runsheet/mesh-cli@abc123def. SHA256SUMS is signed keyless with Sigstore by runsheet/mesh-cli/.github/workflows/release-tag.yml; verify as in runsheet/mesh's README."
export NOTES="$notes"
mirror_up_to_date
export FAKE_RELEASE_EXISTS=0

reset_release_dir
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
echo "ok: v0.2.187 — no release on the mirror, create with no prerelease flag, notes passed through, README and binding unchanged"

# 2. A tag with a "-": --prerelease is added.
reset_release_dir
: >"$log"
./scripts/mirror-release.sh v0.2.188-rc.1 def456abc "$release_dir"
call_with "$log" "release" "create" "--prerelease" >/dev/null || fail "v0.2.188-rc.1 must be published as a prerelease"
echo "ok: v0.2.188-rc.1 — --prerelease"

# 3. The README's content differs: a PUT happens.
reset_release_dir
: >"$log"
mirror_up_to_date
printf 'a stale mirror README' >"$FAKE_REMOTE/README.md"
./scripts/mirror-release.sh v0.2.187 abc123def "$release_dir"
readme_line=$(call_with "$log" "${readme_put[@]}") || fail "README not pushed though its content differs"
[[ $readme_line == *$'\x1f'"sha=sha-of-README.md"* ]] || fail "README PUT does not name the sha it replaces"
call_with "$log" "${binding_put[@]}" >/dev/null && fail "binding pushed though it is identical"
echo "ok: README pushed when its content differs"
export FAKE_README_CONTENT=$(base64 -w0 dist/mirror/README.md)

# 4. A release already exists on the mirror with identical assets: kept,
#    no create, no delete.
reset_release_dir
export FAKE_RELEASE_EXISTS=1
export FAKE_DOWNLOAD_DIR="$existing_identical_dir"
: >"$log"
out=$(./scripts/mirror-release.sh v0.2.187 abc123def "$release_dir" 2>&1)
call_with "$log" "release" "create" >/dev/null && fail "mirror: gh release create ran though an identical release already exists"
call_with "$log" "release" "delete" >/dev/null && fail "mirror: gh release delete ran"
[[ $out == *"release v0.2.187 already on runsheet/mesh with identical assets; keeping it"* ]] || fail "mirror: no keep-it log line: $out"
echo "ok: mirror — existing release with identical assets is kept, no create, no delete"

# 5. A release already exists on the mirror whose SHA256SUMS differs:
#    non-zero, no create.
reset_release_dir
export FAKE_RELEASE_EXISTS=1
export FAKE_DOWNLOAD_DIR="$existing_differing_dir"
: >"$log"
rc=0
./scripts/mirror-release.sh v0.2.187 abc123def "$release_dir" >/dev/null 2>&1 || rc=$?
[[ $rc -ne 0 ]] || fail "mirror: expected a non-zero exit for a differing existing release"
call_with "$log" "release" "create" >/dev/null && fail "mirror: gh release create ran though the existing release differs"
echo "ok: mirror — existing release with a differing SHA256SUMS exits non-zero, no create"

# 5b. A release exists on the mirror missing the Sigstore bundle asset:
#     non-zero, naming it, no create — a mirror release that never got
#     that fifth asset must not be logged as identical.
reset_release_dir
export FAKE_RELEASE_EXISTS=1
export FAKE_RELEASE_ASSETS="mesh-linux-amd64 mesh-linux-arm64 mesh-darwin-arm64 SHA256SUMS"
export FAKE_DOWNLOAD_DIR="$existing_identical_dir"
: >"$log"
rc=0
out=$(./scripts/mirror-release.sh v0.2.187 abc123def "$release_dir" 2>&1) || rc=$?
[[ $rc -ne 0 ]] || fail "mirror: expected a non-zero exit for a release missing the Sigstore bundle"
[[ $out == *"SHA256SUMS.sigstore.json"* ]] || fail "mirror: the missing bundle was not named: $out"
call_with "$log" "release" "create" >/dev/null && fail "mirror: gh release create ran though the existing release is missing the bundle"
echo "ok: mirror — existing release missing the Sigstore bundle exits non-zero naming it, no create"

unset FAKE_DOWNLOAD_DIR FAKE_RELEASE_EXISTS FAKE_RELEASE_ASSETS

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

# 6. The binding differs only by its final newline — the mirror's copy
#    lacks it, then carries an extra one: still a change, so it is PUT.
#    A comparison through $(…) strips trailing newlines and would miss it.
for variant in missing extra; do
  : >"$log"
  mirror_up_to_date
  if [[ $variant == missing ]]; then
    printf '%s' "$(cat "dist/mirror/$binding")" >"$FAKE_REMOTE/$binding"
  else
    printf '\n' >>"$FAKE_REMOTE/$binding"
  fi
  cmp -s "dist/mirror/$binding" "$FAKE_REMOTE/$binding" && fail "test setup: the $variant-newline copy is byte-identical"
  ./scripts/mirror-release.sh v0.2.187 abc123def "$release_dir"
  binding_line=$(call_with "$log" "${binding_put[@]}") || fail "binding not PUT though its final newline is $variant on the mirror"
  [[ $binding_line == *$'\x1f'"sha=sha-of-$binding"* ]] || fail "binding PUT does not name the sha it replaces"
  call_with "$log" "${readme_put[@]}" >/dev/null && fail "README pushed though its content already matches"
done
echo "ok: $binding PUT when it differs only by its final newline"

echo "mirror-release_test.sh: PASS"

# --- The same three cases for the source path: scripts/ensure-release.sh
# called directly as release-tag.yml's "Create release on
# runsheet/mesh-cli" step does, GH_REPO=runsheet/mesh-cli, no TARGET
# (so --verify-tag, not --target main).

export GH_REPO="runsheet/mesh-cli"

# 6. No release yet: create with --verify-tag, no --target.
reset_release_dir
export FAKE_RELEASE_EXISTS=0
: >"$log"
./scripts/ensure-release.sh v0.2.187 abc123def "$release_dir"
create_line=$(call_with "$log" "release" "create" "v0.2.187") || fail "source: no release create for v0.2.187"
[[ $create_line == *$'\x1f--verify-tag'* ]] || fail "source: --verify-tag missing from the create"
[[ $create_line == *$'\x1f--target'* ]] && fail "source: --target must not be passed on runsheet/mesh-cli"
echo "ok: source — no release on runsheet/mesh-cli, create with --verify-tag"

# 7. An identical release already exists: kept, no create, no delete,
#    and the existing Sigstore bundle is pulled over the freshly signed
#    one so the mirror step that follows carries the same bytes.
reset_release_dir
export FAKE_RELEASE_EXISTS=1
export FAKE_DOWNLOAD_DIR="$existing_identical_dir"
: >"$log"
out=$(./scripts/ensure-release.sh v0.2.187 abc123def "$release_dir" 2>&1)
call_with "$log" "release" "create" >/dev/null && fail "source: gh release create ran though an identical release already exists"
call_with "$log" "release" "delete" >/dev/null && fail "source: gh release delete ran"
[[ $out == *"release v0.2.187 already on runsheet/mesh-cli with identical assets; keeping it"* ]] || fail "source: no keep-it log line: $out"
cmp -s "$release_dir/SHA256SUMS.sigstore.json" "$existing_identical_dir/SHA256SUMS.sigstore.json" \
  || fail "source: the existing SHA256SUMS.sigstore.json was not pulled over the freshly signed one"
echo "ok: source — existing release with identical assets is kept, no create, no delete, bundle reused"

# 8. An existing release whose SHA256SUMS differs: non-zero, no create.
reset_release_dir
export FAKE_RELEASE_EXISTS=1
export FAKE_DOWNLOAD_DIR="$existing_differing_dir"
: >"$log"
rc=0
./scripts/ensure-release.sh v0.2.187 abc123def "$release_dir" >/dev/null 2>&1 || rc=$?
[[ $rc -ne 0 ]] || fail "source: expected a non-zero exit for a differing existing release"
call_with "$log" "release" "create" >/dev/null && fail "source: gh release create ran though the existing release differs"
echo "ok: source — existing release with a differing SHA256SUMS exits non-zero, no create"

# 9. An existing release missing one of the three binaries: non-zero,
#    naming the missing asset, no create.
reset_release_dir
export FAKE_RELEASE_EXISTS=1
export FAKE_RELEASE_ASSETS="mesh-linux-amd64 mesh-darwin-arm64 SHA256SUMS SHA256SUMS.sigstore.json"
export FAKE_DOWNLOAD_DIR="$existing_identical_dir"
: >"$log"
rc=0
out=$(./scripts/ensure-release.sh v0.2.187 abc123def "$release_dir" 2>&1) || rc=$?
[[ $rc -ne 0 ]] || fail "source: expected a non-zero exit for a release missing an asset"
[[ $out == *"mesh-linux-arm64"* ]] || fail "source: the missing asset was not named: $out"
call_with "$log" "release" "create" >/dev/null && fail "source: gh release create ran though the existing release is missing an asset"
echo "ok: source — existing release missing an asset exits non-zero naming it, no create"

# 10. An existing release missing the Sigstore bundle asset: non-zero,
#     naming it, no create — otherwise the source path would keep the
#     release as identical and then fail unhelpfully at the later `cp`
#     of the (nonexistent) existing bundle.
reset_release_dir
export FAKE_RELEASE_EXISTS=1
export FAKE_RELEASE_ASSETS="mesh-linux-amd64 mesh-linux-arm64 mesh-darwin-arm64 SHA256SUMS"
export FAKE_DOWNLOAD_DIR="$existing_identical_dir"
: >"$log"
rc=0
out=$(./scripts/ensure-release.sh v0.2.187 abc123def "$release_dir" 2>&1) || rc=$?
[[ $rc -ne 0 ]] || fail "source: expected a non-zero exit for a release missing the Sigstore bundle"
[[ $out == *"SHA256SUMS.sigstore.json"* ]] || fail "source: the missing bundle was not named: $out"
call_with "$log" "release" "create" >/dev/null && fail "source: gh release create ran though the existing release is missing the bundle"
echo "ok: source — existing release missing the Sigstore bundle exits non-zero naming it, no create"

echo "ensure-release.sh: PASS"
