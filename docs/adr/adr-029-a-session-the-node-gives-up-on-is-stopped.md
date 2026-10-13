---
title: "ADR-029: A session the node gives up on is stopped"
summary: A build whose claim is lost — stalled, its lease ended, released or cancelled — is stopped, its unit, process tree and pod killed and its slot freed with nothing sent; capacity counts the build sessions alive on the host, orphans included, and sweeps an orphan; and a build silent for host.starve_after on a host past host.overload_per_core is stopped and released `stalled: host overloaded: …`, charging no attempt.
status: accepted
---

# ADR-029: A session the node gives up on is stopped

2026-10-13, goal `a-stalled-session-is-stopped-not-orphaned` (items 1–4).
Amends ADR-008 (The session's end, the sweep), ADR-009 (adoption) and
ADR-023's the node backs off a hot host; follows mesh-specs ADR 008.

## Context

On 2026-10-13 the grapevine node on dl360 (node.yaml `capacity: 8`) had
nine `claude -p` session processes running while the platform reported
four goals `stalled: no heartbeat for 45 minutes on node
dl360-grapevine` (and dl360-anythink): the-session-floor-test-holds-under-load,
the-pipeline-page-shows-where-prs-wait, delivery-baseline-on-the-scorecard
and grapevine's a-signup-remembers-the-code-that-brought-it. The host
was CPU-saturated — load 81–138 on 96 cores, about 42 cores of
self-hosted GitHub Actions CI and 25 of its e2e Postgres — so sessions
starved, missed their heartbeats, and the platform stalled their items
and put them back in ready.

Three things were wrong at once:

1. **The node kept running what it no longer held.** A stalled item was
   back in ready (or claimed by another node), but its session ran on —
   an `Outlive` unit is detached on a cancelled ctx so a restart can
   adopt it, and nothing cancelled it anyway. The agent CLI, its MCP
   helpers and its pod consumed CPU and a slot for work nobody would
   hand in.
2. **Capacity counted only what the node tracked.** `Running()` is the
   builds this process started or adopted; the orphan above was neither,
   so the node claimed past `capacity: 8` into a host that was already
   drowning.
3. **The item took the blame.** A stall charges an attempt; the reason
   said `no heartbeat for 45 minutes` and nothing of the host, so a
   starved session read as the item's own failure, and enough of them
   would split or fail a goal that had done nothing wrong.

## Decision

**When the node or the platform gives up on a session, the node stops
it — its unit, process tree and pod — and frees its slot. Capacity
counts the build sessions alive on the host. A session the host starved
is released with the host's reason, never failed.**

### A build whose claim is lost is stopped (item 1, #1157)

- Every `ProgressEvery` a build reads its item back
  (`internal/node/lost.go`, `lostByRead`): terminal, cancelled, back in
  ready or pending, or claimed or running on another node id is lost.
  The heartbeat is sent only while the item is held; a heartbeat
  refused as not held (`lostRefusal`: `item is <status>, not running`,
  `not yours`, `held by`, `cancelled`, `no such item`) loses it too — a
  transport error, a timeout or an older platform never does.
- The session's ctx is cancelled with `node.ErrClaimLost`, which `Is`
  `harness.ErrStopSession`: `harness.StopRequested` makes
  `ClaudeCode.follow` kill an `Outlive` unit instead of detaching it (a
  plain cancel — the node shutting down — still detaches for adoption).
  The validation retry turn honours it too.
- `Runner.afterLost` logs `item <label>: claim lost (<why>); session
  stopped` and `logKilled`'s line, removes the session dir, and sends
  **nothing** — no fail, finish or release: the item is not the node's.
  `RunItem`/`AdoptItem` return, so the loop (or `AdoptSessions`'
  goroutine) frees the slot; a sandboxed build's pod goes by its
  deferred `SessionPod.Close`. An adopted build is covered the same way.
- `gatewaytest.Fake.SetItem` sets an item's row as the platform would,
  for the tests.

### Capacity counts live sessions, and sweeps the orphans (item 2, #1158)

- `Loop.Live` (`internal/node/liveproc.go`, `LiveProbe{Systemctl, Root,
  Find, SweepEvery}`) counts the build sessions alive on the host every
  tick: the active `mesh-session-build-*` scopes, else the work root's
  `FoundSessions` of kind build whose pid is alive. Each tick `free =
  Capacity() − max(Running(), live)`; past capacity the node claims
  nothing, logged once per change `capacity: <n> live agent sessions
  (<m> orphans) against capacity <c>; claiming nothing`.
  `Loop.LiveSessions()` reports the figures.
- Every `SweepEvery` ticks (default 5, the first included) an untracked
  live build is judged (`judgeItem`, as the start-up sweep's): an item
  terminal or unknown, or live on another node while a state file of
  this node's names it, is stopped (`stopScope`, shared with
  `SweepScopes`, or `FoundSession.Kill`) and logged `capacity: orphan:
  stopped <unit> (item X, <status> on the platform): killed …`. A live
  item of this node's is counted, not stopped. A scope whose item
  another node holds and no state file of this node's names is a
  sibling node's on the same host (dl360 runs two) — neither counted
  nor stopped. ADR-008's start-up sweep is thus periodic too.

### The host's overload is read (item 3, #1161)

- `MachineReading` also reads `/proc/pressure/cpu`'s `some avg10` and
  `avg60` (optional: absent without PSI) and `PerCore()` is `load1 ÷
  cores`. `MachineReading.Overloaded(HostConfig)` is true past
  node.yaml's `host.overload_per_core` (default 1.0) or, when set,
  `host.overload_cpu_some_avg60`, and its reason names load per core,
  the load, the cores and the pressure: `host overloaded: load/core
  1.44 (138.2 on 96 cores), cpu some avg60 71%`. It is distinct from
  `Hot`, which the scaler's back-off judges by.
- The heartbeat's `scaling.host` also carries `perCore` and
  `cpuSomeAvg60`, each sent only when the platform's
  `HeartbeatNodeInput.scaling.host` type takes it (`takesScaling` probes
  host's own type one level deeper; the fake builds matching nested
  fields).

### A session starved on an overloaded host is released (item 4)

- node.yaml's `host.starve_after` (default 15 minutes, well inside the
  platform's 45-minute stall; `HostConfig.StarveAfter`, reloaded and
  logged `host: starve_after <old> → <new>` like the other thresholds)
  is how long a build may write nothing to its stream. Silence is
  measured from the later of the last event the harness passed on and
  the last write to `<workdir>.session/output.jsonl`, so a long tool
  call that streams nothing the node reports still counts as heard.
- On each heartbeat tick (`internal/node/starve.go`, `checkStarved`,
  after `checkClaim`) a build silent that long is judged against the
  host **at that moment** (`Runner.Machine`, `Runner.Host` =
  `Loop.HostThresholds`). Overloaded, its ctx is cancelled with
  `node.ErrStarved` (an `ErrStopSession`): the unit is stopped and
  killed, the pod closed, the slot freed — and `afterStarved` sends
  `ReleaseWorkItem` with the note `stalled: <Overloaded reason>`. A
  release is the attempt not counted (trd §3). node.log says `item
  <label>: stalled on an overloaded host; released: <reason>`.
- A silent build on a host that is not overloaded runs on as today: the
  node never second-guesses a slow item on a quiet host; the platform's
  own stall still judges it.
- When item 1's check finds that the platform has already stalled the
  item and the host is overloaded at that moment, `afterLost`'s line
  carries the overload: `claim lost (item is ready; host overloaded:
  load/core …); session stopped`, so the operator sees the item was not
  to blame. Nothing is sent: the item is not the node's to note.

## Consequences

- No session outlives the node's hold on its item by more than one
  `ProgressEvery` and the unit's stop grace; an orphan another process
  left is stopped within `SweepEvery` ticks; and the node never claims
  past capacity while orphans run.
- A starved build costs its item no attempt and no lineage step, and
  its release note says why, so an overloaded host cannot fail or split
  a goal by itself. The session's work is cut short — it is killed —
  and the next claim takes it up again (a rework reuses the item's
  worktree and its unpushed commits, ADR-028); on a host that
  stays overloaded, the back-off (ADR-023) has already stopped new
  claims.
- The platform still records its own 45-minute stall as an attempt
  where the node did not get there first (the node gone, or the host
  overloaded only after the platform's window). Carrying the overload
  reason onto the platform's stall — so it too charges nothing — is the
  platform's half (runsheet), owed to a follow-up; this node only logs
  it.
- Only builds are starve-checked and live-counted: evaluations, plans,
  research and rebases do not count against capacity and keep their
  own timeouts.
