#!/usr/bin/env bash
# Tests scripts/mini-owner-reading.sh against a fake `curl` on PATH (the
# fake-gh pattern of mirror-release_test.sh, adapted to one gateway
# endpoint distinguished by the query it carries) serving fixture
# answers under scripts/testdata/mini-owner-reading/: nodeIndexs with
# ownerKind served, the "Cannot query field" refusal and its fallback
# without it, enterpriseIndexs, and workItemIndexs per org and status.
set -euo pipefail
cd "$(dirname "$0")/.."

fail() { echo "FAIL: $*" >&2; exit 1; }

fixtures=scripts/testdata/mini-owner-reading
for f in node-served.json node-unknown-field.json node-fallback.json enterprise.json \
  items-org-a-review.json items-org-b-review.json items-empty.json myorgs.json; do
  [[ -f $fixtures/$f ]] || fail "missing fixture $fixtures/$f"
done

bash -n scripts/mini-owner-reading.sh || fail "bash -n failed"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
bin="$work/bin"
mkdir -p "$bin"

cat >"$bin/curl" <<FAKE
#!/usr/bin/env bash
set -euo pipefail
repo_root="$(pwd)"
data="" prev=""
for a in "\$@"; do
  case \$prev in
    --data-binary) data=\$a ;;
  esac
  prev=\$a
done
query=\$(jq -r .query <<<"\$data")
case "\$query" in
  *'myOrgs'*)
    [[ "\${FAKE_NEEDS_MYORGS:-}" == 1 ]] || { echo "fake curl: myOrgs asked for but FAKE_NEEDS_MYORGS not set" >&2; exit 1; }
    cat "\$repo_root/$fixtures/myorgs.json"
    ;;
  *'nodeIndexs'*'ownerKind'*)
    cat "\$repo_root/\${FAKE_NODE_FIRST:?}"
    ;;
  *'nodeIndexs'*)
    cat "\$repo_root/\${FAKE_NODE_RETRY:?}"
    ;;
  *'enterpriseIndexs'*)
    cat "\$repo_root/$fixtures/enterprise.json"
    ;;
  *'workItemIndexs'*'org-a'*'review'*|*'workItemIndexs'*'review'*'org-a'*)
    cat "\$repo_root/$fixtures/items-org-a-review.json"
    ;;
  *'workItemIndexs'*'org-b'*'review'*|*'workItemIndexs'*'review'*'org-b'*)
    cat "\$repo_root/$fixtures/items-org-b-review.json"
    ;;
  *'workItemIndexs'*)
    cat "\$repo_root/$fixtures/items-empty.json"
    ;;
  *) echo "fake curl: unexpected query: \$query" >&2; exit 6 ;;
esac
FAKE
chmod +x "$bin/curl"

export PATH="$bin:$PATH"

scripts/mini-owner-reading.sh --help | grep -q '^usage: RUNSHEET_TOKEN=' ||
  fail "--help prints no usage"

# 1. RUNSHEET_TOKEN missing: exits non-zero with the usage line.
if out=$(unset RUNSHEET_TOKEN; scripts/mini-owner-reading.sh 2>"$work/err"); then
  fail "missing RUNSHEET_TOKEN exited 0"
fi
grep -q 'RUNSHEET_TOKEN is not set' "$work/err" || fail "missing token did not say why: $(cat "$work/err")"
grep -q '^usage: RUNSHEET_TOKEN=' "$work/err" || fail "missing token printed no usage line: $(cat "$work/err")"
echo "ok: RUNSHEET_TOKEN missing — exits non-zero with the usage line"

# 2. ownerKind served: the three lines, the grapevine row excluded as
#    retired, the owner row grouped with its real ownerKind, and the
#    24h window keeping org-a's claim and dropping org-b's (older).
export RUNSHEET_TOKEN=fake-token
export FAKE_NODE_FIRST="$fixtures/node-served.json"
export FAKE_NODE_RETRY="$fixtures/node-served.json" # unused on this path
out=$(scripts/mini-owner-reading.sh --now "2026-10-03T12:00:00Z" https://gateway.example/graphql runsheet-mini grape-1)
echo "$out"
grep -qx 'grapevine: 0 live Mini row(s)' <<<"$out" || fail "missing the grapevine line (served): $out"
grep -qx 'by owner: 1 live row(s), ownerKind enterprise, ownerId ent-1' <<<"$out" || fail "missing the by-owner line (served): $out"
grep -qx 'claims on anythink orgs: 1 in the last 24h' <<<"$out" || fail "missing the claims line (served): $out"
echo "ok: ownerKind served — the three lines, retired rows excluded, the 24h window applied"

# 3. ownerKind not served: the platform refuses the field, the script
#    retries without it and reports "not served" instead of failing.
export FAKE_NODE_FIRST="$fixtures/node-unknown-field.json"
export FAKE_NODE_RETRY="$fixtures/node-fallback.json"
out=$(scripts/mini-owner-reading.sh --now "2026-10-03T12:00:00Z" https://gateway.example/graphql runsheet-mini grape-1)
echo "$out"
grep -qx 'by owner: 1 live row(s), ownerKind not served, ownerId ent-1' <<<"$out" ||
  fail "ownerKind not served did not fall back: $out"
echo "ok: ownerKind not served — falls back and reports 'not served' rather than failing"

# 4. The grapevine org id, omitted, is resolved from myOrgs by tenant
#    slug/login "grapevine".
export FAKE_NODE_FIRST="$fixtures/node-served.json"
export FAKE_NODE_RETRY="$fixtures/node-served.json"
export FAKE_NEEDS_MYORGS=1
out=$(scripts/mini-owner-reading.sh --now "2026-10-03T12:00:00Z" https://gateway.example/graphql runsheet-mini)
unset FAKE_NEEDS_MYORGS
grep -qx 'grapevine: 0 live Mini row(s)' <<<"$out" || fail "myOrgs resolution path: wrong grapevine line: $out"
echo "ok: an omitted grapevine org id is resolved from myOrgs"

echo "mini-owner-reading_test.sh: PASS"
