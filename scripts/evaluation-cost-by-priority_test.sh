#!/usr/bin/env bash
# A fake `mesh` behind MESH_BIN that echoes a fixed `mesh node usage`
# output (the kind table, the priority rows, the unparsed line) and
# records the arguments it was called with, asserting
# scripts/evaluation-cost-by-priority.sh passes --from/--to/--log/--kind
# through (and --since as --from to tomorrow) and prints only the
# `evaluate <priority>:` rows.
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
evaluate high: 1 items, 0×1 / 2×2, median \$1.40/item, total \$1.40
evaluate normal: 2 items, 2×1 / 0×2, median \$0.30/item, total \$0.60
evaluate unknown: 1 items, 0×1 / 0×2, median \$0.20/item, total \$0.20
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

want='evaluate high: 1 items, 0×1 / 2×2, median $1.40/item, total $1.40
evaluate normal: 2 items, 2×1 / 0×2, median $0.30/item, total $0.60
evaluate unknown: 1 items, 0×1 / 0×2, median $0.20/item, total $0.20'
if [ "$out" != "$want" ]; then
  echo "FAIL: output:"$'\n'"$out"$'\n'"want:"$'\n'"$want" >&2
  exit 1
fi

"$repo_root/scripts/evaluation-cost-by-priority.sh" --since 2026-10-01 --log "$work/node.log" >/dev/null
args="$(cat "$args_file")"
case "$args" in
  "node usage --from 2026-10-01 --to "*" --kind evaluate --log $work/node.log") ;;
  *) echo "FAIL: unexpected --since args: $args" >&2; exit 1 ;;
esac

echo "ok"
