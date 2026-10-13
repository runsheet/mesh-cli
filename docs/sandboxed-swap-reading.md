---
summary: scripts/sandboxed-swap-reading.sh reads a node.log for each self-update swap, the sandboxed sessions running at it, and whether the next process adopted each one and finished its item; this records how to run it on the dl360 and the fixture's own reading, taken on the build machine for item 5 of a-sandboxed-session-outlives-the-node.
---

# sandboxed-swap-reading: how to run it, and what it reads today

`scripts/sandboxed-swap-reading.sh` is a reading (ADR-020's `Measure:`
item): it exits 0 and prints what a node.log shows for a human to read,
never a verdict a build is judged on.

## How to run it

On the dl360, under `systemctl --user` (the node's own log lives at
`$XDG_RUNTIME_DIR`-adjacent `~/.mesh`, but the unit's `HOME` and
`XDG_RUNTIME_DIR` matter for any `mesh` command run alongside it, not
for this script, which only reads a file):

```
XDG_RUNTIME_DIR=/run/user/$(id -u) scripts/sandboxed-swap-reading.sh
```

With no argument it reads `$MESH_HOME/node.log` (`$MESH_HOME` default
`~/.mesh` — ADR-019, `internal/node/logfile.go`'s `ServiceLog`), the
same default `mesh node status` and the other `*-reading.sh` scripts
use. Point it at a specific file, or a rotated one
(`~/.mesh/node.log.1`), with a bare path or `--log`:

```
scripts/sandboxed-swap-reading.sh ~/.mesh/node.log.1
scripts/sandboxed-swap-reading.sh --log ~/.mesh/node.log
```

It prints two sections: `== self-update swaps`, one line per swap
found (its version, when, and whether the node was sandboxed then),
and `== sessions`, one line per build session that was running at a
sandboxed swap — whether the process it restarted into logged adopting
it, whether it then logged finishing the item, and how many seconds
passed between the swap and the first line logging that item adopted.
A session that reads `adopted no` or `finished no` is the stall this
reading exists to catch: `--help` has the exact line shapes.

## Reading

Taken 2026-10-13T20:03Z, on the build machine for this item (not the
dl360 — no node has run there with this goal's sandboxed-adoption code
yet, so there is no real node.log carrying a sandboxed swap to read;
the dl360 reading belongs to the goal and is recorded there, once a
node built past this goal's items runs through a self-update on it,
whatever it shows), against the fixture:

```
scripts/sandboxed-swap-reading.sh scripts/testdata/sandboxed-swap-node.log
```

Output as printed:

```
== reading taken 2026-10-13 20:03:38 +0000 on mesh-a-sandboxed-session-outlives-the-node-5-3a6bdbf7-0
log: scripts/testdata/sandboxed-swap-node.log

== self-update swaps
swap 1: v0.2.241 at 2026/10/12 04:00:01 (sandboxed, 2 declared)
swap 2: v0.2.242 at 2026/10/12 05:30:02 (sandboxed, 1 declared)

== sessions
swap 1 v0.2.241: item sandbox-swap/1: adopted yes (16s), finished yes
swap 1 v0.2.241: item sandbox-swap/2: adopted yes (21s), finished yes
swap 2 v0.2.242: item sandbox-swap/3: adopted no (-), finished no
```

Honest notes on this reading:

- The fixture (`scripts/testdata/sandboxed-swap-node.log`) is built
  from the exact lines the node itself writes — `self-update: <to>
  verified; no drain: swapping and restarting, sessions left running
  for the new binary to adopt (sandbox: on)` and `self-update: <to> is
  in place; exiting for the service to start it, <n> session(s) left
  for it to adopt` (`internal/node/selfupdate.go`'s `Loop.update` and
  `sandboxNote`), `item <label>: build on …` (`internal/node/runner.go`,
  `RunItem`), `item <label>: adopted its running session (container
  …, from byte …)` (`runner.go`'s `AdoptItem`), and `item <label>:
  pushed <sha>; finished <branch> ($<cost>)` (`runner.go`'s
  `afterBuild`) — not invented text, so the script is proven against
  the same shapes a real dl360 node.log carries.
- Swap 1 carries two sessions that both get adopted (16s and 21s after
  the swap — the window ADR-009's "the item heartbeated at once so the
  stall window never elapses" is about) and both finish: the ordinary
  case this goal's acceptance ("an upgrade mid-session leaves the
  session running and adopted, finishing on the new node with no
  stall") describes.
- Swap 2 carries one session (`sandbox-swap/3`, open since before the
  swap, sandboxed because an earlier swap in the same log already
  showed it — or, here, another item — adopted with `container`) that
  the fixture deliberately never shows adopted or finished afterward:
  the stalled case the script exists to surface without a human
  reading the whole file by hand. This is the fixture's own invented
  failure, not an observation of a real stall.
- This is not the goal's own dl360 observation ("a dl360 upgrade
  observed with sessions carried across (node.log)"): that reading is
  the goal's, taken on the dl360 itself once a node built past this
  goal's items has gone through a real self-update there, not this
  item's. This item's acceptance is that the script exists, runs
  against a fixture of a known shape, and is tested — proven above and
  by `go test ./scripts/...`.
