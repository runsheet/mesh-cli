#!/usr/bin/env bash
# Builds a fixture MESH_HOME (node.yaml, node.log, node.log.1, an old
# node.log.2, sessions.log, node.pid) with fixed mtimes, a stub of the
# node's running binary behind a fixture /proc/<pid>/exe and another `mesh`
# first on PATH that knows no `version`, as the mini's ~/.local/bin/mesh
# does, and asserts the version is the running binary's and scripts/node-reload-reading.sh prints the whole capacity line in
# the window, the reload error, the start line in force and the size line.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

home="$work/mesh"
mkdir -p "$home" "$work/bin" "$work/node-bin" "$work/proc/2868134"
cat >"$work/node-bin/mesh" <<'EOF'
#!/usr/bin/env bash
echo "mesh v0.2.222 (commit ddc8c4e)"
EOF
chmod +x "$work/node-bin/mesh"
ln -s "$work/node-bin/mesh" "$work/proc/2868134/exe"
echo 2868134 >"$home/node.pid"
# An older mesh first on PATH, as on the mini: it has no `version`.
cat >"$work/bin/mesh" <<'EOF'
#!/usr/bin/env bash
echo 'Error: unknown command "version" for "mesh"' >&2
exit 1
EOF
chmod +x "$work/bin/mesh"

cat >"$home/node.yaml" <<'EOF'
orgs: [runsheet]
capacity: 8
EOF
touch -d "2026-10-02 06:43:57" "$home/node.yaml"

cat >"$home/node.log.2" <<'EOF'
mesh node: 2026/09/29 11:00:00 capacity 4 → 6
EOF
touch -d "2026-09-29 23:59:54" "$home/node.log.2"

cat >"$home/node.log.1" <<'EOF'
mesh node: 2026/09/30 08:00:00 capacity 5 → 6
mesh node: 2026/10/01 14:00:00 mesh node v0.2.219, pid 100, config /home/op/.mesh/node.yaml
mesh node: 2026/10/01 15:10:00 reload config: my orgs: context canceled
EOF
touch -d "2026-10-01 16:00:00" "$home/node.log.1"

{
  echo "mesh node: 2026/10/02 06:40:00 mesh node v0.2.222, pid 2868134, config /home/op/.mesh/node.yaml"
  echo "mesh node: 2026/10/02 06:44:12 capacity 6 → 8"
  echo "mesh node: 2026/10/02 06:44:13 a session said capacity 6 → 8 in passing"
  head -c 7200 /dev/zero | tr '\0' 'x'
  echo
} >"$home/node.log"
size="$(stat -c %s "$home/node.log")"
touch -d "2026-10-02 10:00:00" "$home/node.log"

echo "session text" >"$home/sessions.log"

"$repo_root/scripts/node-reload-reading.sh" --help | grep -q '^usage: scripts/node-reload-reading.sh' || {
  echo "FAIL: --help prints no usage" >&2
  exit 1
}

out="$(PATH="$work/bin:$PATH" MESH_HOME="$home" MESH_PROC="$work/proc" "$repo_root/scripts/node-reload-reading.sh" --hours 24 --now "2026-10-02 10:00:00")"

echo "$out"

fail() {
  echo "FAIL: $1" >&2
  exit 1
}

grep -qx "binary: $work/proc/2868134/exe (the running node, pid 2868134 in node.pid, -> $work/node-bin/mesh)" <<<"$out" \
  || fail "the binary is not the running node's"
grep -qx "mesh version: mesh v0.2.222 (commit ddc8c4e)" <<<"$out" || fail "missing the running binary's mesh version"
grep -q "unknown command" <<<"$out" && fail "the version came from the mesh first on PATH"
grep -qx "in force: mesh node: 2026/10/02 06:40:00 mesh node v0.2.222, pid 2868134, config /home/op/.mesh/node.yaml" <<<"$out" \
  || fail "missing the start line in force"
grep -qx "mtime: 2026-10-02 06:43:57.000000000 $(date -d '2026-10-02 06:43:57' +%z)" <<<"$out" || fail "missing node.yaml mtime"
grep -qx "capacity: 2:capacity: 8" <<<"$out" || fail "missing node.yaml capacity line"
grep -qx "node.log: mesh node: 2026/10/02 06:44:12 capacity 6 → 8" <<<"$out" || fail "missing the whole capacity 6 → 8 line"
grep -qx "node.log.1: mesh node: 2026/10/01 15:10:00 reload config: my orgs: context canceled" <<<"$out" \
  || fail "missing the reload config line"
grep -q "in passing" <<<"$out" && fail "a line that only mentions capacity was printed"
grep -q "capacity 5 → 6" <<<"$out" && fail "a line before the window was printed"
grep -q "capacity 4 → 6" <<<"$out" && fail "an old rotated file was read"
grep -qx "(node.log.2 last written 2026-09-29 23:59:54.000000000 $(date -d '2026-09-29 23:59:54' +%z), before the window: not read)" <<<"$out" \
  || fail "missing the skipped node.log.2 line"
# node.log began at node.log.1's mtime, 16:00:00 the day before: 18 h.
grep -qx "node.log: $size bytes in 64800 s since node.log.1 (2026-10-01 16:00:00), $((size * 3600 / 64800)) bytes/hour" <<<"$out" \
  || fail "missing the size line"
grep -q "sessions.log$" <<<"$out" || fail "missing ls -l of sessions.log"
grep -qx "no $home/token or no $work/proc/2868134/exe: mesh node status not run" <<<"$out" || fail "missing the status skip"

# MESH_BIN, when set, wins over the running node's binary.
out="$(PATH="$work/bin:$PATH" MESH_HOME="$home" MESH_PROC="$work/proc" MESH_BIN="$work/node-bin/mesh" "$repo_root/scripts/node-reload-reading.sh" --hours 24 --now "2026-10-02 10:00:00")"
grep -qx "binary: $work/node-bin/mesh (MESH_BIN)" <<<"$out" || fail "MESH_BIN not used"

echo "PASS"
