---
summary: A session's unit and output file, not the node process, own a session — a node shutting down detaches from its host Claude sessions (Job.Outlive) instead of stopping them, and on start, before its first claim, the node finds every `<workdir>.session/state.json` naming it, heartbeats each item at once, follows each session from its saved offset (or reads the file of one that ended while no node ran) and runs the same tail the live path runs — push and FinishWorkItem, the verdict, the plan, the research, the rebase — removing the session directory only once the end is handled; the heartbeat carries the optional `adopted [{itemId, kind, since}]`.
status: accepted
---

# ADR-009: Sessions outlive the node

## Context

mesh-specs ADR 008 decides that a session does not belong to the node
process that started it. mesh-cli ADR-008 gave each host session its
own unit (a transient systemd user scope, else a detached process
group), its stream-json in `<workdir>.session/output.jsonl` and a
secret-free `state.json` beside it. But the node still stopped the unit
when its context was cancelled — a node shutting down killed its
sessions as before — and nothing read a session another process had
started. This ADR records the second step of goal
`node-upgrades-keep-sessions`: the node lets go of its sessions when it
stops, and the next one adopts them.

## Decision

**The session's unit and its output file own the session. The node
process that started it is only its first reader.**

- **Letting go.** A build, evaluate, plan, research or rebase job is
  started with `Job.Outlive`. A cancelled context — the node shutting
  down (SIGTERM, `systemctl --user restart`, a self-update's exit) —
  then leaves the unit running and the session directory in place, and
  `Run` returns `ctx.Err()` at once; only the job's own `Timeout` stops
  the unit. The session's temporary `CLAUDE_CONFIG_DIR` and
  `GH_CONFIG_DIR` are left to it too (`unlessDetached`), their paths in
  `state.json` as `tempDirs`. A sandboxed or Codex session still ends
  with its context: the pod becomes the unit in a later goal.
- **The offset.** `state.json`'s `offset` is saved after every event
  the node has read and passed on (item 1 saved it at most once a
  second), so an adopting node reports nothing twice.
- **Finding them.** On `mesh node start`, in both modes, before the
  first tick, `FindSessions(workRoot, nodeIDs)` walks the work root for
  `*.session/state.json` whose `nodeId` is one of the node's ids (one
  per org served), not searching inside a git checkout.
- **Adopting.** `Loop.AdoptSessions` marks each as the loop's own — a
  build in `running` (so it counts in `Running()` and against capacity),
  an evaluation, plan, research or rebase in its own map, every one in
  the drain's `sessions()` — so none is claimed or started again, and
  `Loop.Adopt`, which runs a held item from the start, skips it. Every
  item's heartbeat (`ReportWorkItemProgress`, with the session id) is
  sent there and then, before the first tick's claims, so the
  platform's stall window never elapses across a restart. `AdoptWith`
  hands each to its kind's adopter: `Runner.AdoptItem`,
  `Evaluator.AdoptEvaluation`, `Planner.AdoptPlan`,
  `Director.AdoptResearch`, `Rebaser.AdoptRebase`. Each checks that
  the subject is still this node's (the item running here, in
  evaluating here, its rebase claimed; the goal in planning) — one that
  is not has its session stopped and removed.
- **Attaching.** `harness.Attacher` (`ClaudeCode.Attach`, forwarded by
  `ObserveLimits` and `ObserveSpend`) reads `state.json`, follows
  `output.jsonl` from the start — passing progress on only for events
  past the saved offset, so the final `Result` is the whole session's —
  while the pid lives, keeps saving the offset, and stops the unit at
  the state's own timeout from `startedAt`. The build and rebase
  adopters re-issue the item's token onto the checkout and run the same
  heartbeat ticker (progress text, cost deltas, session id, token
  renewal) as a session this process started.
- **The dead session.** A session whose pid is gone ended while no
  node ran: Attach reads its file to the end. A result event ends it as
  it would have (a build is pushed and finished); none is an
  unsuccessful end carrying the output's last text (a build fails with
  it).
- **One tail.** The "after the session" half of each kind is one
  function the live path and the adoption path both call:
  `afterBuild` (the limit release, fail, push, `FinishWorkItem`, the
  ended goal's document), `afterEvaluate` (the verdict, the second
  opinion, `EvaluateWorkItem`), `readPlan` and `handIn` (`PlanGoal`),
  the director's `handIn` (`RecordDirectionResearch`, an empty one when
  unreadable) and `afterRebase` (`PushedHead`, `FinishWorkItemRebase` or
  `FailWorkItemRebase`). Each returns at once on a cancelled context,
  leaving the session for the next node; otherwise it removes the
  session directory and its temp dirs (`harness.RemoveSession`) once
  the item is finished or failed.
- **The heartbeat.** `gateway.HeartbeatExtras.Adopted`
  (`[]AdoptedSession{ItemID, Kind, Since}`) is the optional heartbeat
  field `adopted: [{itemId, kind, since}]`, sent only when set, like
  `draining`: the adopted sessions that have not yet ended. `itemId` is
  a plan's goal id and a research session's direction id.

## Limits

- An adopted session has no `SessionAccount`: no usage record is
  handed in for the part the adopting node read, and the account's
  secret is not known to redact from its text. The first node records
  its usage as it detaches.
- An adopted evaluation has no second opinion: its prompt is not kept.
  The notes say `Codex second opinion unavailable: the evaluation was
  adopted after a restart`.
- An adopted plan whose acceptance an evaluator cannot see (adr-004) is
  handed in as it stands; it is not asked again.
- The pid in `state.json` is trusted: a pid reused by another process
  after a reboot reads as alive until that process ends.
- Stopping the node while a session is still being prepared (before the
  unit starts) leaves the item for `Loop.Adopt`, as before.

## Consequences

- A node restart kills no host session and loses no finished work; the
  item's heartbeat goes on across it.
- `mesh node upgrade` and the self-update restart at once instead of
  draining, and `SIGHUP` restarts a node in place (adr-002, amendment
  of 2026-09-26).
- Stopping a node no longer fails a claimed rebase; the next node
  finishes it.
- Test fakes record each call's time (`gatewaytest.Call.At`).
