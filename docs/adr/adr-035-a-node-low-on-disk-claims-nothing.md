---
title: "ADR-035: A node low on disk claims nothing"
summary: Every tick, before any claim, the node reads the free space on node.yaml's disk.path (the work root when left out) and, under disk.free_min_gb (default 10 GiB), claims nothing of any kind — build, evaluation, rebase, plan, research, merge batch or main validation — logging `disk_low: <path> <free> free < floor <floor>; claiming nothing` once as it enters the state and `disk ok: …; claiming again` once as it leaves it; running sessions run on, and while low the loop carries a critical `disk_low` Finding for the quartermaster's report.
status: accepted
---

# ADR-035: A node low on disk claims nothing

2026-10-16, goal `a-node-low-on-disk-claims-nothing-and-a-missing-tool-is-named`
(items 2 and 3). Sits beside [ADR-023](adr-023-the-node-backs-off-a-hot-host.md)
(the node backs off a hot host) and
[ADR-031](adr-031-a-sessions-load-matches-its-cpu-limit.md), and shares
the quartermaster's disk reading and Finding
([ADR-034](adr-034-the-quartermaster-keeps-the-node-fit.md), goal
`the-quartermaster-keeps-the-nodes-fit`, its item 6 still to land when
this was written).

## Context

On 2026-10-15 between 09:38 and 09:45 local both nodes ran with almost no
free disk (node.log on each) and kept claiming. Each new claim made a
worktree, a session directory, a pod's HOME and a package install, and
each one failed for space: clones and fetches, `go build`'s cache,
npm/pnpm installs, podman's layers. Every failure was reported as the
item's — a validation that could not write, a workspace that could not be
prepared — and the items were reworked, released and claimed again on the
same full disk, spending attempts on a condition of the host.

ADR-023 stops new claims on a hot host, but heat is read from load and
temperature; nothing read the disk. The quartermaster (goal
`the-quartermaster-keeps-the-nodes-fit`, item 1) brought a `DiskCheck` —
percentage-based, warn under 15% free and critical under 5%, reported as
a Finding on its own cadence — but it reports, it does not hold claims,
and it was not yet in the loop.

## Decision

- **A floor in bytes.** node.yaml `disk: {free_min_gb: 10, path: <the
  work root>}` (`DiskConfig`; `free_min_gb` left out or zero is
  `DefaultDiskFreeMinGB`, 10; `path` left out is the loop's `WorkRoot`,
  `~/.mesh/work`). A negative floor is refused at load naming
  `disk.free_min_gb`. It is reloaded every tick like `host:`, a change
  logged `disk: free_min_gb 10 → 20` / `disk: path <old> → <new>`. An
  absolute floor, not a percentage: a build needs gigabytes whatever the
  size of the filesystem, and 5% of a large disk is plenty while 5% of a
  small one is not.
- **Read every tick, before any claim** (`Loop.diskFloor`,
  `internal/node/diskfloor.go`, after `hostBackoff`): statfs(2) on the
  path through the quartermaster's `StatfsFunc` (`Loop.Statfs`, the real
  one when nil — injectable, so tests fake the free space), its
  `DiskStat.Free` against `FloorBytes()`.
- **Under the floor the node claims nothing of any kind.** `Tick` skips
  `evaluateHeld`, `rebaseRequested`, `planRequested`, `researchDue`,
  `validateBatches` (merge batch candidates and main validation requests)
  and `claimReady`: nothing is listed for a claim and nothing is sent.
  This is wider than a hot host's hold, which stops only new builds:
  every kind writes a checkout, a session directory or a pod to disk.
  The heartbeat, adoption and the sweeps go on.
- **Running sessions are never touched.** A session already running
  finishes or fails on its own; stopping it would throw away work that
  may still fit.
- **Said once per crossing.** `disk_low: <path> 3.0GiB free < floor
  10.0GiB; claiming nothing` as the node enters the state — at its first
  tick when it starts under the floor — and `disk ok: <path> 40.0GiB
  free >= floor 10.0GiB; claiming again` as it leaves it. A failed read
  keeps the last judgement and logs `disk: cannot read <path>: <err>;
  keeping the last judgement` once per distinct error.
- **A Finding while low.** `Loop.DiskFindings()` is one Finding of kind
  `disk_low`, severity `critical`, its subject the path, its bytes the
  free space and its detail the `disk_low` line — the quartermaster's
  shape, for its report and `node.findings` once its loop wiring (its
  item 5) folds it in; none above the floor.

## Consequences

- A full disk costs no attempts: the items stay ready, evaluating or
  requested on the platform for another node or for this one once space
  is freed, and the next tick after it is claims again with no restart.
- The node's own reason is node.log and, through the quartermaster, the
  findings. The heartbeat carries nothing new: the platform sees a node
  that claims nothing, as it sees a hot or draining one, until the
  quartermaster's `findings` reach it.
- The floor is per node, not per session: two nodes on one host read the
  same filesystem and both stop. Freeing space — the quartermaster's
  scratch and dead-checkout checks, or the operator — lets both claim
  again.
- 10 GiB is a guess sized for a build plus its pod and installs; a node
  that builds larger repos raises it in node.yaml.

## Tests

`internal/node/diskfloor_test.go`: three ticks under the floor over a
build, an evaluation, a plan, a rebase and a direction claim nothing and
log one `disk_low`, then claim once freed and log one `disk ok`; a merge
batch is not claimed under the floor; a node starting under the floor
says so at its first tick; `disk:` parses, defaults, refuses a negative
and reloads. `internal/node/disklow_e2e_test.go`'s `TestDiskLowE2E`: the
node's own Loop under `Run` against the fake gateway and a real git
origin, with a ready item, an evaluation and a merge batch candidate
waiting — ten ticks under the floor send no claim mutation of any kind
and log `disk_low` once; once space is freed it logs `disk ok` and claims
the build, the evaluation and the batch.
