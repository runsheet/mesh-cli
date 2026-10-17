---
title: "ADR-034: The quartermaster keeps the node fit"
summary: Every tick the Loop runs its Quartermaster — at most once per Every, ten minutes — four checks (disk free under 15%/5% warn/critical; scratch, stale runsheet-*/latchkey-*/go-build*/mesh-* dirs under /tmp past six hours reaped plus the sandbox's home sweep; dead_checkout, worktrees of merged, done, cancelled, superseded or unknown items idle an hour removed; version, a node behind its desired release past an hour warn, failing the same update for two critical), each a Finding {kind, severity, subject, detail, bytes, reclaimed, at} left in ~/.mesh/node.findings, printed by `mesh node status` and sent as the heartbeat's `findings` when HeartbeatNodeInput takes it; dl360's /etc/cron.d/mesh-tmp-clean and /etc/cron.hourly/mesh-home-sweep go once a node runs it.
status: accepted
---

# ADR-034: The quartermaster keeps the node fit

2026-10-17, goal `the-quartermaster-keeps-the-nodes-fit` (items 1–6).
Amends [ADR-007](adr-007-the-sandbox-on-the-node.md)'s start-up sweep
(it now runs on the quartermaster's cadence as well) and builds on
[ADR-028](adr-028-one-clone-per-repo-one-worktree-per-item.md) (one
worktree per item) and [ADR-002](adr-002-signed-tag-releases.md)'s
self-update.

## Context

On 2026-10-15 the dl360's disk filled. `/tmp` held 67G of `runsheet-*`
dirs left by test runs, beside stale `latchkey-*` and `go-build*` dirs
and `mesh-home-*` sandbox HOMEs; the work root held worktrees of items
merged days before. With no space, every build failed on its
environment and the merge queue deadlocked: no validation could write
its checkout. Two stop-gaps kept it running — `/etc/cron.d/mesh-tmp-clean`
(the `/tmp` leftovers) and `/etc/cron.hourly/mesh-home-sweep` (ADR-007's
sweep, hourly) — root's crons that no other node has and nobody sees
the output of. Nothing on the node said the disk was filling, and the
platform could not tell a full node from a slow one. A node stuck
behind its desired release (an update that kept failing) was equally
silent until someone read node.log.

## Decision

The node looks after itself. `internal/node/quartermaster.go`'s
`Quartermaster{Checks, Every}` runs a list of `Check`s — `func(ctx,
now) []Finding` — and each check reports what it found as
`Finding{Kind, Severity, Subject, Detail, Bytes, Reclaimed, At}`,
severity `ok`, `warn` or `critical`. A check that panics is caught into
one `critical` finding of kind `quartermaster` naming it (`check N`,
`panicked: <value>`), and the rest still run.

### The four checks

`mesh node start` builds them in `nodeQuartermaster`
(`internal/commands/nodecmd.go`):

- **disk** (`quartermaster_disk.go`, `DiskCheck`, `NewDiskCheck`): one
  finding per filesystem among the work root, `os.TempDir()` and the
  home directory, de-duplicated by device (statfs's fsid; `Statfs`
  injectable). `warn` under `DiskWarnFree`, 15% free; `critical` under
  `DiskCriticalFree`, 5%. `Bytes` is the free bytes; detail `<pct>%
  free (<free> of <total> bytes free)`. It reads only; it reclaims
  nothing — the other checks do.
- **scratch** (`quartermaster_scratch.go`, `ScratchCheck`): reaps the
  entries directly under `/tmp` matching `ScratchPrefixes` —
  `runsheet-*`, `latchkey-*`, `go-build*`, `mesh-*` — that are
  directories owned by the node's own uid and whose newest mtime
  anywhere inside is older than `ScratchMinAge`, six hours; a read-only
  tree (Go's module cache) is made owner-writable and removed. It
  **never touches** a symlink (never followed), another user's entry, a
  path `InUse` names, an entry younger than six hours, or a
  `mesh-home-*` dir — that is left to the sandbox's own sweep, which
  knows which HOME a running container mounts: with a sandbox the check
  runs `SweepSandbox` (ADR-007's start-up sweep, `sandbox.SweepKeeping`)
  on every run, keeping every session `FindSessionsIn` finds. One
  `scratch` finding, `reaped <n> dirs, <bytes>; <k> in use kept` (and
  `; sandbox: <sweep line>`), `Reclaimed` the bytes removed, `Bytes`
  what is left past the age; `warn` past `ScratchWarnBytes`, 20G left
  over, and one more `warn` per path a removal failed on.
- **dead_checkout** (`quartermaster_checkouts.go`,
  `DeadCheckoutCheck`): every item workspace under the work root —
  `<goal>/<seq>` and `<goal>/<seq>-eval` — whose item the platform says
  is merged, done, already_done, cancelled or superseded, or does not
  know, is removed by `Git.RemoveWorkspace` (`git worktree remove
  --force` + prune under the repo lock), and worktrees whose directory
  is already gone are pruned from each shared clone. It **never
  touches** a checkout a session uses (`Loop.LiveCheckout`: its
  `<dir>.session` is there, or an adopted session's), one touched within
  `DeadCheckoutIdle`, an hour, one whose item is still live, the shared
  clone itself, the goal doc's `<goal>/0`, plan and research checkouts —
  and nothing at all when the platform cannot answer (`warn`, `the
  platform could not answer for <n> checkouts, kept`). One finding, `<n>
  dead found, <m> removed, <bytes> bytes reclaimed, <k> kept because
  live; <p> prunable worktrees pruned`; `warn` on a removal that failed.
- **version** (`quartermaster_version.go`, `VersionCheck`): reads the
  running version and commit, the platform's desired version
  (`Loop.desiredVersion`, unchanged) and the last self-update failure.
  `ok` at the desired version or with none known; `warn` once the
  desired version has differed for `VersionWarnAfter`, an hour (the
  self-update's own retry), or at once for a dev build where a release
  is desired; `critical` when the same desired version has failed to
  update for `VersionCriticalAfter`, two hours, the error in the
  detail. It reclaims nothing and never updates: the self-update does.

### Cadence and where findings go

The Loop runs it from `Tick`, after the heartbeat
(`Loop.Quartermaster`, `runQuartermaster`): `RunIfDue` runs the checks
only when `Every` — `DefaultQuartermasterEvery`, ten minutes — has
passed since the last run, and otherwise returns that run's findings.
Each run's findings:

- go to `Loop.Findings`, which `mesh node start` sets to
  `RunState.WriteFindings`: `~/.mesh/node.findings`, written atomically,
  removed when empty and by `Stopped`;
- are logged, each non-ok one `quartermaster: <kind> <severity>
  <subject>: <detail>` once while it is unchanged, and each reclaim
  `quartermaster: reclaimed <bytes> (<kind>)`;
- ride every heartbeat until the next run.

### `mesh node status`

While a node runs here (the pidfile's process alive), `mesh node
status` prints `node.findings` as a `findings:` block
(`node.FindingsLines`), critical first, then warn, then ok, by kind
within each:

```
findings:
  disk critical /home/mesh/.mesh/work: 4% free (4294967296 of 107374182400 bytes free)
  version warn v1.0.0: v1.0.0 (abc1234): desired v1.1.0, differing for 1h1m0s
  dead_checkout ok /home/mesh/.mesh/work: 1 dead found, 1 removed, 2232 bytes reclaimed, 0 kept because live; 0 prunable worktrees pruned
  scratch ok /tmp: reaped 3 dirs, 12.0K; 0 in use kept
```

or `findings: none` before the first run.

### The heartbeat's `findings`

`HeartbeatExtras.Findings` → `findings: [{kind, severity, subject,
detail, bytes, reclaimed, at}]` (`gateway.Finding`), the latest run's.
It is sent only when the gateway's `HeartbeatNodeInput` has the field
— `Client.TakesHeartbeatFindings`, asked by introspection and kept
`ScalingProbeEvery` (an hour) like `commit`, a failed probe a no — since
a platform without it would refuse the whole heartbeat. Without it the
node says once per run `platform has no heartbeat findings; kept on the
node`; node.findings and `mesh node status` still have them. The
platform half — storing them per node and the health page showing them
— is runsheet's.

### The crons go

`/etc/cron.d/mesh-tmp-clean` and `/etc/cron.hourly/mesh-home-sweep` on
the dl360 (evidence: the disk-full of 2026-10-15) are removed once a
node there runs a release with the quartermaster: the scratch check does
what the first did, with an age guard and an owner check it lacked, and
the sandbox sweep on its cadence does what the second did.

## Consequences

- A filling disk is a `critical` finding in node.log, `mesh node status`
  and on the platform before builds fail on it; the node reclaims its
  own leftovers every ten minutes rather than an operator's cron.
- The scratch check reaps only names the node's own work leaves; a
  leftover under another name still fills the disk, and the disk check
  says so.
- The sandbox sweep now runs every ten minutes, not only at start. A
  session between `Sandbox.Open` and its agent's first container has a
  pod with no running container and no `state.json` yet; a sweep that
  lands in that window can remove its pod and HOME, and that session
  then fails as a pod that went away does. An age guard on the sweep is
  owed if it is seen.
- A dead checkout is removed on the platform's word; an outage keeps
  everything (the check warns), never removes.
- `go test -tags e2e ./internal/node/ -run Quartermaster`
  (`quartermaster_e2e_test.go`) runs it end to end on the Loop and the
  fake gateway: a node full of leftovers — a disk at 4% free, stale and
  fresh `/tmp` dirs, a merged item's and a live item's worktree, a
  desired version ahead — reports disk critical, scratch and
  dead_checkout with bytes reclaimed and version warn in node.findings,
  `mesh node status`'s block and the heartbeat, removing only the stale
  dirs and the merged worktree.
