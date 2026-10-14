---
summary: scripts/evaluate-once-reading.sh counts `item <label>: evaluate on` lines per item and `not yours` lines across one or more node.logs, flagging any item evaluated more than once; this records how to run it and the fixture's own reading, taken on the build machine for item 4 of an-items-evaluation-runs-on-one-node (the dl360 logs were not readable from this session).
---

# evaluate-once-reading: how to run it, and what it reads today

`scripts/evaluate-once-reading.sh` is a reading (ADR-020's `Measure:`
item): it exits 0 and prints what one or more node.logs show for a
human to read, never a verdict a build is judged on.

## How to run it

On the dl360, against both of its node logs (`/home/anythink/.mesh/node.log`
and `/home/grapevine/.mesh/node.log`):

```
scripts/evaluate-once-reading.sh /home/anythink/.mesh/node.log /home/grapevine/.mesh/node.log
```

Narrow to a window with `--since`/`--until`, in node.log's own
`YYYY/MM/DD HH:MM:SS` format:

```
scripts/evaluate-once-reading.sh --since "2026/10/15 06:35:00" --until "2026/10/15 07:10:00" \
  /home/anythink/.mesh/node.log /home/grapevine/.mesh/node.log
```

It prints two sections: `== evaluate on, by item`, one line per item
label naming how many `item <label>: evaluate on` lines (internal/node/evaluate.go's
`e.logf`, one per evaluation session started) were found for it across
every log given, flagging any count over one with `MORE THAN ONCE` —
two nodes both starting an evaluation of the same item, the race this
goal's items are about — and `== not yours`, the count of lines
containing the words `not yours` across the same logs, the refusal a
losing claim, progress report or attestation gets once another node
holds the item (`--help` has the exact line shapes).

## Reading

Taken 2026-10-14 on the build machine for this item: the dl360's
`/home/anythink/.mesh/node.log` and `/home/grapevine/.mesh/node.log`
named in the goal's notes (for 2026-10-15 06:35-07:10 local, item
a-goal-is-done-when-its-last-item-merges/1) are not readable from this
session — this session has no `/home/anythink` and no node.log under
`/home/grapevine/.mesh` — so this records the fixture's own reading
instead, against `scripts/testdata/evaluate-once-anythink.log` and
`scripts/testdata/evaluate-once-grapevine.log`:

```
scripts/evaluate-once-reading.sh scripts/testdata/evaluate-once-anythink.log scripts/testdata/evaluate-once-grapevine.log
```

Output as printed:

```
== reading taken 2026-10-14 20:24:22 +0000 on mesh-an-items-evaluation-runs-on-one-node-4-a0dc437b-0
logs: scripts/testdata/evaluate-once-anythink.log scripts/testdata/evaluate-once-grapevine.log

== evaluate on, by item
ship-it/1: 2  MORE THAN ONCE
ship-it/2: 1
ship-it/3: 1

== not yours
not yours: 2
```

Honest notes on this reading:

- The fixture logs are built from the exact line shapes the node
  itself writes: `item <label>: evaluate on <family>: <branch>`
  (`internal/node/evaluate.go`'s `e.logf`, both of its two call sites —
  the already-done claim form and the diff-bytes form), and three
  distinct real `not yours` refusal shapes found in this repo's own
  tests and code — `reportWorkItemProgress: not yours`
  (`internal/node/lost_test.go`) and
  `recordValidationAttestation: … is not the head of … (GitHub has
  …): not yours` (`internal/node/attest_newer_head_test.go`) — not
  invented text, so the script is proven against the same words a real
  dl360 node.log carries.
- `ship-it/1` is logged once in each fixture log (`evaluate on Claude
  opus` in anythink's, `evaluate on Codex gpt-6-luna` in grapevine's):
  two nodes each starting their own evaluation of the one item, the
  race this goal exists to close. `ship-it/2` and `ship-it/3` are each
  evaluated once, in one log only — the ordinary case — and are not
  flagged.
- Each fixture log carries one `not yours` line (the loser's progress
  report in anythink's, the loser's attestation in grapevine's), so the
  combined count is 2, matching this item's acceptance.
- This is not the goal's own dl360 observation ("after release, both
  dl360 node logs show one `evaluate on` per item and zero `not yours`
  lines over an hour"): that reading belongs to the goal and is
  recorded there, taken on the dl360 itself once a node built past this
  goal's items has run there for an hour, whatever it shows. This
  item's acceptance is that the script exists, runs against a fixture
  of a known shape, and is tested — proven above and by `bash
  scripts/evaluate-once-reading_test.sh`.
