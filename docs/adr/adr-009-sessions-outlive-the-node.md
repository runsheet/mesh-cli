---
summary: A session's unit and output file, not the node process, own a session — a node shutting down detaches from its Claude sessions (Job.Outlive) instead of stopping them, a host session in its scope and a sandboxed one in its pod (its agent run detached, state.json naming the pod, container and HOME, so a sandboxed node's self-update no longer drains), and on start, before its first claim, the node finds every `<workdir>.session/state.json` naming it, heartbeats each item at once, follows each session from its saved offset (or reads the file of one that ended while no node ran) and runs the same tail the live path runs — push and FinishWorkItem, the verdict, the plan, the research, the rebase — removing the session directory only once the end is handled; the heartbeat carries the optional `adopted [{itemId, kind, since}]`, with `goalId`, `directionId`, `sessionId` and `alive` beside them, re-read every heartbeat and sent only when the platform's schema takes them; and claims, not only sessions, outlive the node — a plan or research claim is recorded under `<work root>/.claims/` the moment it is granted, removed once its session's end is handled, and a starting node re-claims each one with no session (same node id, inside PlanClaimTTL) and runs it fresh, one log line each.
status: accepted
---

# ADR-009: Sessions outlive the node

(Amended 2026-10-03: Claims — claims, not only sessions, outlive the
node; and the heartbeat — `adopted` carries `goalId`, `directionId`,
`sessionId` and `alive` beside `itemId`, `kind` and `since`. Amended
2026-10-13: Sandboxed sessions — a sandboxed session outlives the node
in its pod and is adopted like a host one, and the self-update no
longer drains a sandboxed node. Amended 2026-10-14: Token files — an
adopting node keeps an adopted session's mesh-context token file
fresh.)

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
  `state.json` as `tempDirs`. A sandboxed session's pod is its unit
  and outlives the node the same way (Sandboxed sessions, amendment
  2026-10-13, below); a Codex session still ends with its context.
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
  (`[]AdoptedSession{ItemID, Kind, Since, GoalID, DirectionID,
  SessionID, Alive}`) is the optional heartbeat field `adopted:
  [{itemId, kind, since}]`, sent only when set, like `draining`: the
  adopted sessions that have not yet ended. `itemId` is a plan's goal
  id and a research session's direction id, kept as-is for an older
  platform. Beside it, so a person watching can tell an adopted plan or
  research session from a dead claim: `goalId` on a plan entry and
  `directionId` on a research entry (both equal to `itemId`), the
  agent's own session id once its session's `state.json` names one, and
  whether its unit still runs. These four are sent only when the
  gateway's `HeartbeatNodeInput`'s `adopted` element type takes them —
  introspected and cached an hour like the other ADR-004 probes
  (`takesAdoptedFields`); an older schema gets the entry as before,
  `{itemId, kind, since}` alone. `Loop.Adopted()` re-reads each adopted
  session's `state.json` for `sessionId` and calls its unit's `Alive`
  on every call, so both the id the agent names after its session opens
  and the session ending reach the very next heartbeat, not only the
  one the restart itself sent.

## Claims (amendment 2026-10-03)

**Claims, not only sessions, outlive the node.** A session's state file
appears only once its unit opens, after `Credentials.Open` and the
context fetch; a plan or research claim taken before then had nothing on
disk, so a node that exited for an upgrade in between left the platform
holding a claim nobody worked until `PlanClaimTTL` (30 minutes) let it
go — the grapevine node on 2026-10-03, four plans claimed at 03:30:35Z,
two swaps, nothing planned until 04:00:44Z.

- **The record.** `internal/node/claims.go`: right after
  `ClaimPlan` / `ClaimDirectionResearch` succeeds and before the
  session's goroutine starts, the loop writes a `ClaimRecord {nodeId,
  kind (plan|research), goalId | directionId, scopeId, name, claimedAt}`
  atomically to `<work root>/.claims/<kind>-<subject>.json`
  (`Loop.ClaimsDir`, `ClaimsDir(root)`) — a dot dir, never a checkout or
  a session dir, so `FindSessions` passes it by.
- **Its lifetime.** The record is removed when the session's end is
  handled: the goroutine that ran `Plan` / `Research` removes it once it
  returns — the plan or research handed in, a plan released, a hold
  (`ErrNoCredential`, `ErrNoHeadroom`) — unless its context is done
  (`unlessDetached`): a node letting go of its sessions (`Restart`,
  `ErrRestartForUpdate`, a shutdown) keeps the record for the next
  process. An adopted plan or research session's end removes it the same
  way. A record is not a session: the drain's exit test does not count it.
- **The re-open.** On `mesh node start`, after `AdoptSessions` and once
  the loop's planner and director are set, `FindClaims(root, nodeIDs)`
  lists the records naming the node's ids and `Loop.ReopenClaims(ctx,
  claims, found)` takes each one whose subject has no `FoundSession`
  (one that has is `AdoptSessions`'): it reads the goal (still in
  planning) or the direction (still live), claims it again **by the same
  node id** — the platform lets its holder re-claim within the TTL
  (runsheet goal.go) — logs one line, `re-opening the plan claim of goal
  <slug> held since <t>` or `re-opening the research claim of
  <project>/<direction> held since <t>`, and runs it as a fresh session
  through the same marking (`planning`, `researching`) the tick's pass
  uses. A refused re-claim, a goal no longer in planning, a direction no
  longer live, or a node id no org here serves under removes the record
  and logs why; a read that fails leaves it for the next start. A
  draining node re-opens nothing.
- **What survives a swap**, then: open sessions are adopted; unopened
  plan and research claims are re-opened; builds and evaluations are
  re-run from the platform's record — a held item with no session by
  `Loop.Adopt`, an item in evaluating on this node by the next tick.
  No claim file is written for a build or an evaluation: the platform's
  record is theirs. `swap_e2e_test.go` proves research and evaluations
  across a self-update's swap, `claims_test.go` plans.
- **The log says which way an update goes.** A node's self-update
  logs `self-update: <to> verified; no drain: swapping and restarting,
  sessions left running for the new binary to adopt`; a sandboxed
  node's line ends ` (sandbox: on)`, since its pods are carried across
  too. Until 2026-10-13 a sandboxed node drained first instead
  (Sandboxed sessions, below).
- **The proof** is the grapevine replay (`hotswap_plans_test.go`):
  four plans claimed, the binary swapped — once through a release
  signed on a virtual Sigstore and verified by the `ReleaseUpdater` —
  before any session opened, and again after two had; the new process
  adopts the open two, re-opens the rest by the same node id, and every
  plan reaches `planGoal` inside `PlanClaimTTL`, no claim re-taken.
  `gatewaytest.Fake` holds the platform's rule for it: `planClaimedAt`
  stamped on a fresh claim and kept on the holder's re-claim, another
  node refused while `PlanClaimTTL` (settable, default 30 minutes) has
  not passed since it.

## Sandboxed sessions (amendment 2026-10-13)

Goal `a-sandboxed-session-outlives-the-node`. Until now a sandboxed
session ran its agent attached (`podman run -i`, the node holding its
pipes), wrote no `state.json`, and ended with its node; a sandboxed
node's self-update therefore drained first (`DrainAndWait`), waiting
out its longest build. The pod is now the session's unit, as a scope is
a host session's (mesh-cli ADR-007's amendment of the same date):

- **Letting go.** A sandboxed `Job.Outlive` job (`ClaudeCode.Sandbox`
  set and `DetachSandboxed`, which `mesh node start` turns on with a
  sandbox) starts its agent detached — `Session.StartDetached`,
  `podman run -d` in the session's pod, its stdin the prompt in
  `<workdir>.session/.prompt` (removed after the agent's first event),
  its stdout and stderr `output.jsonl` and `stderr.log` — and writes
  `state.json` with `runner: podman` (`sessionunit.SandboxRunner`), no
  pid (`0`), `unit` and `container` the agent's container
  (`sandbox.AgentName`), and its `pod` and `home`. A cancelled context
  returns `ctx.Err()` and `Session.Detach` leaves the pod, its sidecars
  and the HOME standing; the job's own timeout stops the container at
  `startedAt + timeout`.
- **Finding and judging.** `FindSessionsIn(root, ids, sb)` gives each
  found session the node's sandbox: a sandboxed one is alive by its
  container (`Runtime.AgentState`), `Kill` stops it (`StopAgent`), and
  `Remove` — every adopter's "no longer this node's" path — is
  `harness.RemoveSandboxedSession`: the container stopped, its pod and
  HOME removed with the session directory. The start-up sweep keeps
  every found session's pod, containers and HOME
  (`SweepSandbox` → `sandbox.SweepKeeping`), and the live probe
  (ADR-029) counts a sandboxed build by its container.
- **Attaching.** `ClaudeCode.Attach` follows `output.jsonl` from the
  saved offset while the container runs (`WaitAgent`), and at its end
  removes the pod and HOME (`Sandbox.Reattach`). `Runner.AdoptItem`
  reattaches them as the build's own pod instead
  (`Sandbox.Reattach`, else `ReattachHome` when the pod is gone;
  `harness.WithPodKept`), so ADR-014's validation runs in that session
  and the pod and HOME go only after the finish, unless the node lets
  go again.
- **The self-update no longer drains a sandboxed node.** `Loop.update`
  takes one path whatever the sandbox: verify, ensure the image, swap,
  and `ErrRestartForUpdate` at once, its no-drain line ending
  ` (sandbox: on)`. `DrainAndWait` is no longer called by the loop.
  `mesh node update` still drains (it always did, sandbox or not), and
  `mesh node upgrade`'s `--force` for a sandboxed node is unchanged
  here.
- **The proof** is `internal/node/sandboxed_swap_e2e_test.go`
  (`TestSandboxedSelfUpdateMidSessionFinishesOnTheNewNode`) on
  `sandbox.Fake` and `gatewaytest.Fake`: a sandboxed build running when
  the release updater swaps, `Run` returning without waiting, the
  agent's container still running, the next loop adopting it, no gap
  between the item's heartbeats (`gatewaytest.Call.At`) past the stall
  window, and the item pushed and finished by the new loop with one
  claim. `sandbox_adopt_test.go` proves the find, sweep, adopt and
  remove paths; `selfupdate_test.go`'s
  `TestSelfUpdateSandboxedSwapsWithoutDraining` the update itself.
- Channel mode and Codex sessions are not in a pod and still end with
  the node.

## Token files (amendment 2026-10-14)

Goal `a-sessions-mcp-token-outlives-the-session`
([ADR-029](adr-029-a-sessions-mcp-token-is-refreshed-by-the-node.md)).
A session's mesh-context is a relay reading its bearer per request from
`mesh-context.token` in the session's own directory, which the node
that started it keeps fresh. A session that outlives that node would
otherwise be left on a file nobody rewrites, and lose its tools at the
next rotation, so:

- **state.json names the file.** `sessionunit.State.TokenFile`
  (`tokenFile`) is the path — `<workdir>.session/mesh-context.token` on
  the host, `<HOME>/mesh-context.token` in a pod — never the bearer.
- **A detach leaves it.** A session the node lets go releases its file
  without removing it (`release(false)`), so the relay keeps a good
  bearer until the token's own life is out.
- **The adopter keeps it.** `ClaudeCode.Attach` — host and sandboxed
  alike, under every adopter (`Runner.AdoptItem`,
  `Evaluator.AdoptEvaluation`, `Planner.AdoptPlan`,
  `Director.AdoptResearch`, `Rebaser.AdoptRebase`) — calls its
  `Tokens.Keep(st.TokenFile)` (`keepAdopted`) as it takes the session
  up, so the adopting node's `node.TokenFiles` rewrites the file from
  its own bearer while it follows, removes it once the end is handled,
  and leaves it again on a second detach. The harness's
  `TestAdoptedSessionTokenFileIsRefreshed` proves it.
- A session a node adopts from before this amendment has no
  `tokenFile`: its mesh-context is the old header and keeps nothing.

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
- Stopping the node while a build is still being prepared (before the
  unit starts) leaves the item for `Loop.Adopt`, as before; a plan or
  research being prepared is re-opened (Claims, below).

## Consequences

- A node restart kills no host session and loses no finished work; the
  item's heartbeat goes on across it.
- `mesh node upgrade` and the self-update restart at once instead of
  draining, and `SIGHUP` restarts a node in place (adr-002, amendment
  of 2026-09-26).
- Stopping a node no longer fails a claimed rebase; the next node
  finishes it.
- Test fakes record each call's time (`gatewaytest.Call.At`).

## Amendment 2026-10-17: a host session's temp dirs include its scratch

[ADR-036](adr-036-a-host-sessions-scratch-dies-with-the-session.md)
(goal a-host-sessions-scratch-dies-with-the-session). A host build,
evaluation, plan, rebase or research session now runs with a scratch dir
of its own under `<work root>/.scratch`
(`sessionScratch`, `<kind>-<subject>-<rand>`) as TMPDIR and GOTMPDIR,
with GOCACHE the node's shared build cache. `sessionTempDirs` puts that
dir in `SessionMeta.TempDirs`, so `state.json` names it beside the
session's other temp dirs. The node that handles the session's end —
the one that started it, the one that adopted it after a detach, or the
one that gave up on it — removes it with `harness.RemoveSession`, and
`removeTree` copes with a read-only Go module tree. A scratch whose
session died with a killed node (a validation, a Codex session, or a
session whose `state.json` is gone) is named by no found session, so the
start-up scratch sweep (ADR-036, ADR-007's amendment 2026-10-17)
removes it. A detached
session's scratch outlives the node with the session, and the sweep
keeps it because the found session names it.
`host_scratch_e2e_test.go` proves both paths.
