#!/usr/bin/env bash
# Tests scripts/attestation-reading.sh against a fake `gh` on PATH that
# answers two canned pull-request runs of runsheet/runsheet's test.yml —
# one whose verify job answered verdict: verified and skipped the three
# e2e shards, one whose verify job answered verdict: missing and ran
# them — and checks the two summary lines read 'verified: 1/2' and
# 'shards skipped on a verified verdict: 1/2'. HOME is pointed at an
# empty directory so no real ~/.mesh/token is picked up for the
# scorecard read, which is not under test here.
set -euo pipefail
cd "$(dirname "$0")/.."

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

bin="$work/bin"
mkdir -p "$bin"
mkdir -p "$work/home"

cat >"$work/runs.json" <<'EOF'
[
  {"databaseId": 1001, "headSha": "verifiedsha1", "url": "https://github.com/runsheet/runsheet/actions/runs/1001", "conclusion": "success"},
  {"databaseId": 1002, "headSha": "missingsha22", "url": "https://github.com/runsheet/runsheet/actions/runs/1002", "conclusion": "success"}
]
EOF

cat >"$work/jobs-1001.json" <<'EOF'
{"jobs": [
  {"name": "verify", "databaseId": 2001, "conclusion": "success"},
  {"name": "shard (e2e 1/3)", "databaseId": 2002, "conclusion": "skipped"},
  {"name": "shard (e2e 2/3)", "databaseId": 2003, "conclusion": "skipped"},
  {"name": "shard (e2e 3/3)", "databaseId": 2004, "conclusion": "skipped"},
  {"name": "shard (rest)", "databaseId": 2005, "conclusion": "success"}
]}
EOF

cat >"$work/jobs-1002.json" <<'EOF'
{"jobs": [
  {"name": "verify", "databaseId": 2101, "conclusion": "success"},
  {"name": "shard (e2e 1/3)", "databaseId": 2102, "conclusion": "success"},
  {"name": "shard (e2e 2/3)", "databaseId": 2103, "conclusion": "success"},
  {"name": "shard (e2e 3/3)", "databaseId": 2104, "conclusion": "success"},
  {"name": "shard (rest)", "databaseId": 2105, "conclusion": "success"}
]}
EOF

mkdir -p "$work/logs"
cat >"$work/logs/2001.log" <<'EOF'
2026-10-03T12:00:00.0000000Z verify-attestation  verdict: verified, skip_e2e: true, skip_jobs: integration
EOF
cat >"$work/logs/2101.log" <<'EOF'
2026-10-03T12:00:00.0000000Z verify-attestation  verdict: missing, skip_e2e: false
EOF

cat >"$bin/gh" <<FAKE
#!/usr/bin/env bash
set -euo pipefail
case "\${1:-}" in
  run)
    case "\${2:-}" in
      list)
        cat "$work/runs.json"
        ;;
      view)
        run_id=\$3
        job_id=""
        want_log=0
        want_json=""
        prev=""
        for a in "\$@"; do
          [[ \$prev == "--job" ]] && job_id=\$a
          [[ \$prev == "--json" ]] && want_json=\$a
          [[ \$a == "--log" ]] && want_log=1
          prev=\$a
        done
        if [[ \$want_log == 1 ]]; then
          cat "$work/logs/\$job_id.log" 2>/dev/null || true
        elif [[ -n \$want_json ]]; then
          cat "$work/jobs-\$run_id.json"
        fi
        ;;
    esac
    ;;
esac
FAKE
chmod +x "$bin/gh"

export PATH="$bin:$PATH"
export HOME="$work/home"

out=$(./scripts/attestation-reading.sh runsheet/runsheet test.yml 24)

echo "$out" | grep -qx 'verified: 1/2' ||
  fail "'verified: 1/2' not found in output:
$out"
echo "$out" | grep -qx 'shards skipped on a verified verdict: 1/2' ||
  fail "'shards skipped on a verified verdict: 1/2' not found in output:
$out"
echo "$out" | grep -q '^scorecard: unavailable' ||
  fail "no scorecard line with no ~/.mesh/token in HOME:
$out"

echo "ok: two runs — one verified with shards skipped, one missing without — summarise as 1/2 and 1/2"
echo "attestation-reading.sh: PASS"
