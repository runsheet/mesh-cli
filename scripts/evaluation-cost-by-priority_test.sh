#!/usr/bin/env bash
# A fake `mesh` behind MESH_BIN that echoes a fixed `mesh node usage`
# output (the kind table, the priority section, the unparsed line) and
# records the arguments it was called with, asserting
# scripts/evaluation-cost-by-priority.sh passes --from/--to/--log/--kind
# through and prints only the priority section.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

args_file="$work/args"
cat >"$work/mesh" <<EOF
#!/usr/bin/env bash
echo "\$@" > "$args_file"
cat <<'OUT'
window 2026-10-01..2026-10-02  records 2026-10-01T09:00:00..2026-10-01T10:00:00
kind      sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes  documents fetched median  fetched by tool
evaluate  3         50000                50000              80.0          -                     0                   -                         0

evaluation cost per item by priority
priority  items  evaluations  cost total cents  cost per item median cents  cost per item mean cents  two evaluators %
high      1      2            140               140                        140                        100.0
normal    2      2            60                30                         30                         0.0
unknown   1      1            20                20                         20                         0.0
unparsed 0
OUT
EOF
chmod +x "$work/mesh"

export MESH_BIN="$work/mesh"
out="$("$repo_root/scripts/evaluation-cost-by-priority.sh" --from 2026-10-01 --to 2026-10-02 --log "$work/node.log")"

args="$(cat "$args_file")"
case "$args" in
  "node usage --from 2026-10-01 --to 2026-10-02 --kind evaluate --log $work/node.log") ;;
  *) echo "FAIL: unexpected args: $args" >&2; exit 1 ;;
esac

if ! head -n1 <<<"$out" | grep -qx "evaluation cost per item by priority"; then
  echo "FAIL: missing heading as the first line:"$'\n'"$out" >&2
  exit 1
fi
if ! grep -q "^unknown" <<<"$out"; then
  echo "FAIL: missing the unknown row:"$'\n'"$out" >&2
  exit 1
fi
if grep -q "^unparsed" <<<"$out"; then
  echo "FAIL: output still carries the unparsed line:"$'\n'"$out" >&2
  exit 1
fi

echo "ok"
