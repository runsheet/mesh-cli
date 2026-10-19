---
summary: How mesh-cli starts implementing mesh-specs ADR 008 — internal/sessionunit runs a host claude session in a unit of its own (a transient `systemd-run --user --scope`, else a detached setsid process group; the sandbox's pod is the unit later), its stream-json in `<workdir>.session/output.jsonl`, its stderr in `stderr.log`, and a secret-free `state.json` (node, item/goal/direction, claim, kind, model, account, pid, unit, startedAt, offset) written atomically, read back by a Follower that tails the file line by line; ClaudeCode.Run keeps its result and error semantics, and a cancelled context or timeout stops the unit. A stop (`Unit.Kill`) ends the whole unit, not its leader: a scope's cgroup processes are listed, SIGTERM then SIGKILL go to the cgroup, `systemctl --user stop` stops the scope (KillMode=control-group), the node waits until it is gone (a `Show` that errors is Kill's error, never taken as gone; every signal's and the stop's error is returned), and every process found besides the session's own pid is returned to the caller as what the agent left running; every end of a host session but a detach kills its unit, the Result's `Killed` reports those processes and the node logs them and notes them on the item; `SystemdScope.Systemctl` is injectable, and `ParseUnitName` reads a scope's kind and subject back.
status: accepted
---

# ADR-008: Sessions in their own unit

## Context

mesh-specs ADR 008 decides that a session does not belong to the node
process that started it: it runs in its own unit, writes its output to
a file beside the work dir, and the next node process adopts it. Until
now `ClaudeCode.Run` started `claude -p` with `exec.CommandContext` and
read its stdout through a pipe, so the session was the node's child, in
the node service's cgroup, and died with it. This ADR records the first
step: the session's own unit, its output file and its state file. The
adoption on start, `mesh node upgrade` and the heartbeat's `adopted`
come with later items of goal `node-upgrades-keep-sessions`.

## Decision

- **`internal/sessionunit`.** A `Unit` has `Start(Spec) → Handle{PID,
  UnitName}`, `Alive(Handle)` and `Stop(Handle)`. Two implementations:
  - `SystemdScope`: `systemd-run --user --scope
    --unit=mesh-session-<kind>-<item|goal|direction>-<rand> --collect
    --quiet -- claude …`. `--scope` execs the command in place, so the
    pid is claude's and it inherits the files and environment it is
    given; its cgroup is the user manager's, not the node service's, so
    `systemctl --user stop|restart` of the node leaves it running.
  - `Detached`: the process in a session of its own (`setsid`), so a
    signal to the node's process group or the node's exit does not
    reach it. The fallback for macOS (with launchd's
    `AbandonProcessGroup`) and a Linux without a user systemd.
  `Default()` picks the scope when `systemd-run` is on PATH and
  `systemctl --user show-environment` answers. Both are process groups
  of their own; stopping one ends the whole unit (below). The sandbox's pod (ADR 007) is the
  third unit, added by `sandboxed-sessions` behind the same interface;
  sandboxed sessions still run through `runSandboxed` as before.
- **The session directory** is the work dir's sibling,
  `<workdir>.session/` (a temporary directory for a job with none), so
  nothing lands in the git checkout. `output.jsonl` is the session's
  stdout and `stderr.log` its stderr, both files the process holds
  itself — never a pipe the node's exit would break. The prompt is
  written to a file there, opened as the session's stdin and unlinked
  before the session starts, so it is never left on disk.
- **`state.json`**, written atomically (a temporary file, then a
  rename), holds `nodeId`, `itemId`/`goalId`/`directionId`, `attempt`
  (the claim), `kind`, `label`, `model`, `account`, `harness`,
  `runner`, `pid`, `unit`, `workspace`, `startedAt`, `timeoutSeconds`,
  `maxBudgetUsd`, `sessionId` and `offset`. It never holds the prompt,
  the environment, the item's token or any secret. The node fills it
  from `Job.Meta` (`harness.SessionMeta`), an optional field: a job
  without it still runs. The account is `Meta.Account`, else the job's
  `MESH_ACCOUNT`.
- **`Follow(ctx, path, offset, alive)`** tails `output.jsonl` from an
  offset until the session is gone and the file has stopped growing.
  It hands out at most one line per `Read`, so `ParseStream`'s scanner
  has read exactly through the event it is handling and `Offset()` is
  the end of that event's line. `Run` saves that offset into
  `state.json` after every event (ADR-009; at first at most once a
  second), so a later node can resume from a line boundary.
- **`ClaudeCode.Run`** starts the unit, writes the state, follows the
  output through `ParseStream` (progress callbacks unchanged) and
  stderr into the redacted `Stderr` writer, waits for the exit, and
  ends exactly as before: `UnavailableError` from stderr or an
  event-less stdout, a non-zero exit with a result event as an
  unsuccessful Result, redaction of the job's secrets, and
  `ctx.Err()` on a timeout or cancellation — which now `Stop` the unit
  explicitly. The session directory is removed when `Run` returns: the
  session has been handled. A node that dies mid-`Run` leaves it for
  the adopting node.

## Stop and Kill

Amended 2026-09-28 (goal `a-finished-session-leaves-no-process`). A
session's scope once outlived it by 36 hours on the mini, holding a
`go run ./cmd/runsheet -addr :18211` and its bash the agent had
backgrounded: the old `Stop` signalled claude's process group and asked
`systemctl --user kill` only while claude's own pid was alive, so
whatever was left in the cgroup after claude exited stayed.

- **`Unit.Kill(h) (killed []Process, err error)`** ends the unit, not
  the leader; `Stop(h)` is `Kill` without the report. `Process` is
  `{PID, Comm, Cmdline}`, named from `/proc/<pid>/comm` and `cmdline`
  (cut to 200 bytes).
- **A scope** is stopped as a unit. `systemctl --user show -p
  LoadState,ActiveState` first: a scope not found, inactive or failed —
  the ordinary case after a clean exit, `--collect` having unloaded
  it — is nothing to do, no error and nothing killed. Otherwise its
  `ControlGroup` is read and `/sys/fs/cgroup<ControlGroup>/cgroup.procs`
  listed; `systemctl --user kill --signal=SIGTERM` goes to the whole
  cgroup, `--signal=SIGKILL` once `StopGrace` (5s) has passed with any
  process left, then `systemctl --user stop <unit>.scope` — a transient
  scope's KillMode is control-group (the mini: `KillMode=control-group`,
  `SendSIGKILL=yes`, `TimeoutStopUSec=90s`), and the SIGKILL before it
  means a process ignoring SIGTERM never makes the node wait the 90s.
  Kill then **waits until the unit is gone** (not-found, inactive or
  failed) for `StopGrace` plus 3s; a unit still there is an error
  naming it.
- **Errors are Kill's, never proof of a stop** (amended again for item
  23, after the Codex evaluation of item 17): only a `Show` that
  answers `LoadState=not-found` or `ActiveState` inactive/failed means
  gone — a `Show` that errors (the user manager unreachable) is Kill's
  error, before any signal and at any point of the wait. Reading the
  `ControlGroup` or its `cgroup.procs` failing is Kill's error before
  any signal. A failed SIGTERM, SIGKILL or stop is always returned
  (joined, naming the unit), whatever a later `Show` says, and a `Show`
  failing after it is joined in too. The one answer that is not a
  failure is the manager saying the unit is **not loaded**
  (`ErrNotLoaded`: `systemctl stop` exits 5, `kill` says `not loaded.`)
  — an emptied scope `--collect` unloaded after the SIGTERM, which is
  the unit gone, as a `Show` answering not-found is.
- **The report**: `killed` is every process that was in the cgroup
  besides the session's own pid — what the agent should have cleaned
  up — each `Process.String()` as `comm[pid] cmdline`. It is returned
  only once the unit is confirmed gone; an error before that returns
  none. The caller logs it (the session's end, the start-up sweep: later
  items of the goal).
- **A detached session** is its process group as before: its members
  are listed from `/proc` (a `stat` whose pgrp is the session's pid;
  none on macOS) and reported the same way, SIGTERM to the group, and
  SIGKILL after `StopGrace` while the session or any of its group is
  left. Kill then waits 3s more for the group to be gone (its zombies
  not counted); a refused signal or a group still there is the error,
  and no report.
- **`SystemdScope.Systemctl`** is the `Systemctl` interface — `Show`,
  `Kill`, `Stop`, `ListScopes` (`systemctl --user list-units
  'mesh-session-*' --plain --no-legend --all`, for the start-up sweep)
  and `Processes(cgroup)` — `HostSystemctl` by default, a fake in the
  tests. `ParseUnitName` is `UnitName`'s inverse: kind and subject of
  `mesh-session-<kind>-<subject>-<8 hex>`, the subject as it is in the
  name (`:` and `/` already `_`).

## The start-up sweep

Amended 2026-09-29 (goal `a-finished-session-leaves-no-process`). The
36-hour leak above would have been caught by any restart, had a restart
looked. `node.SweepScopes` runs in `mesh node start`'s `adoptSessions`
right after `FindSessions` and before `AdoptSessions` — whether or not
any session was found — and only when `sessionunit.Default()` is a
`SystemdScope` (a detached session has no scope to list).

- **The rule.** Every scope `ListScopes` names is looked at. An
  inactive or failed one is systemd's (`--collect` unloads it) and
  skipped. One whose unit a found session's `state.json` names is left:
  it is about to be adopted. Otherwise `ParseUnitName` gives its kind
  and subject (a name that does not parse is logged and left) and the
  platform is asked: build, evaluate and rebase by `WorkItem(subject)`,
  plan by `Goal(subject)`. Research is left with a log line — a
  direction is read through its project, which the name does not carry.
  A subject is **live** only in a status a session runs under
  (`SessionLive`: an item claimed, running or evaluating, or one whose
  `rebaseState` is claimed; `GoalLive`: a goal planning) and its scope
  is left, logged `sweep: <unit> left, item <id> is running on the
  platform`. Anything else — ready, review, merged, failed, cancelled,
  done, already_done — and a subject the platform does not know
  (`gateway.ErrNotFound`) is **terminal**: the scope is killed as a
  unit (`SystemdScope.Kill` with no pid, so every process in the cgroup
  is reported) and logged `sweep: stopped <unit> (item <id>, merged on
  the platform): killed go[…] go run …, bash[…]`; the start logs
  `sweep: <n> leaked scope(s) stopped` when any.
- **An outage is not a leak.** Any other error reading the subject
  leaves the scope with a log line and never kills it; a live session
  whose node restarted mid-outage must not be ended by it. The next
  start sweeps again.
- **What `mesh node status` shows.** After the heartbeat and the node
  list, on a machine with a user systemd, `node.ScopeReport` lists every
  mesh-session scope `HostSystemctl.ListScopes` names — its unit, kind,
  subject and the platform's answer, judged by the sweep's own rule:
  `live (item running)`, `terminal (item merged)`, `unknown (item not
  on the platform)`, `research`, `unparsed`, `unread: <err>`, or an
  inactive scope's state — and ends `sessions: <n> live scope(s), <m>
  leaked`. Only scopes of live sessions count as live; terminal and
  unknown ones are leaked; research, unparsed, unread and inactive ones
  are neither. The listing stops nothing: the sweep, at the next start,
  does.

## The session's end

Amended 2026-09-29 (same goal). Claude's own pid exiting is not the end
of what the session started, so **every end of a host session kills its
unit**: `ClaudeCode.Run` and `ClaudeCode.Attach` call `Unit.Kill` once
the leader has exited (or been stopped) and the output has been read to
its end — the normal end (a clean exit), a non-zero exit, an
`UnavailableError`, a cancelled ctx, the job's `Timeout`, an output
error (the session's `output.jsonl` that cannot be opened to follow), a
state-write failure (`state.json` not written after `Start`), the
hand-back's stop (the first run and the resumed one, each its own unit)
and `Attach`'s end of an adopted session alike. The one end that makes
no kill is the detach, below. The stops a cancel, a timeout or the hand-back make *are* that
kill: one `Kill` per unit, whichever end comes first, its report kept.

- **The report**: `Result.Killed` (`[]sessionunit.Process`) is what the
  kill found — the processes the agent left running that the node had
  to kill; nil when none. A handed-back Result's is both runs', and
  `Result.KilledRuns` keeps each run's apart (`KilledPerSession`). A
  session whose `state.json` cannot be written is never followed: its
  unit is killed at once and the Result, returned with the error, still
  carries the kill's report.
- **Not on a detach**: an `Outlive` job whose parent ctx is done is left
  running for the next node and its unit is not killed; the adopting
  node's `Attach` kills it at its real end.
- **A failed kill is never the session's failure**: it is written to
  `ClaudeCode.Stderr` (`harness claude-code: kill unit …`), or, with
  none, carried on `Result.KillError`. `finishClaude`'s result and
  error semantics are unchanged.
- **The node says it — the log line**: each session's tail —
  `afterBuild`, `afterEvaluate` (the second opinion's session and the
  one that stands in for a blind first attempt included), the planner's
  `readPlan`, the director's `handIn`, `afterRebase` — passes
  `Result.KilledPerSession()` and `Result.KillError` to `logKilled`
  (`internal/node/killed.go`), which logs each session's non-empty kill
  as one line, a handed-back build's two runs as two:

      <subject>: session end killed <n> process(es) the agent left running: <pid> <comm>, <pid> <comm>

  (`KilledNote`; the subject is `item <label>` for a build, an
  evaluation and a rebase, `goal <slug>` for a plan,
  `<project>/<direction>` for research), and a failed kill as
  `<subject>: kill: <KillError>`. Nothing is logged when it killed
  nothing. This line is the goal's acceptance: the node log names what
  it killed. Each tail logs it *first*, before its done-ctx return: a
  ctx whose deadline passed has its unit killed like any timeout, and
  only a detach (which kills nothing, so logs nothing) leaves the
  session for the next node. The note is owed only where the platform takes one:
  1. **A build** sends the sentence (a line per session) as a progress
     note on the item (`ReportWorkItemProgress`), beside its usage and
     `tools: …` notes.
  2. **A rebase** sends it the same way, on the rebase's heartbeat.
  3. **An evaluation** puts it in the verdict's notes when this node
     process records a verdict. One that records none — a timeout, an
     error, an unavailable or blind attempt, no verdict — **only logs
     it**: the platform takes no progress note on an item in
     evaluating, so none is sent. The node process holds the sentence
     (`Evaluator.LeftRunning`, per item, for the process's life) and the
     item's next verdict it records carries it; a restart loses it, the
     log line stays.
  4. **A plan and a research session log only**: there is no item to
     note it on.
- **An adopted session no longer the node's**: `FoundSession.Kill`
  replaces the stop that ran only while claude was alive — an item,
  plan or direction the platform no longer has on this node has its
  unit killed whether or not claude still runs (what the agent left
  there may outlive it), and what it killed is logged the same line
  (`stopFound`) — in each adopter's terminal check (`AdoptItem`,
  `AdoptEvaluation`, `AdoptPlan`, `AdoptRebase`) and in `AdoptWith`'s
  drop of a session whose kind has no adopter.
- Sandboxed and Codex sessions are not in a unit and report nothing.

## The work sweep

Amended 2026-10-19 (goal `the-node-sweeps-finished-workspaces`, the
mesh-cli half of runsheet goal `a-finished-items-workspace-is-deleted`).
The start-up sweep stops leaked scopes and touches no files; nothing
deleted a workspace, and the mini's `~/.mesh/work` held 144 goal
directories, 54 GB, on a disk 81% full. `node.PlanWorkSweep(ctx, gw,
root, orgIDs, live, now, logf)` (`internal/node/worksweep.go`) judges
every entry under the work root by the platform's answer — each served
org's `Goals`, slug to goal, then each present goal's `GoalWorkItems`,
seq to item — and returns a `WorkSweepPlan`; `node.ApplyWorkSweep`
removes it. The planning reads and removes nothing.

- **The layout it knows** (never guessed): `<goal-slug>/<seq>` an
  item's checkout, `<seq>.session` its session dir,
  `<seq>.attestation.json` its attestation copy, `<seq>-eval` and
  `<seq>-rebase` (and their `.session`) its evaluation's and rebase's
  checkouts, `<goal-slug>/plan` (and `plan.session`) the plan,
  `<goal-slug>/0` the goal doc's checkout (it goes with its goal), and
  `kept-sessions/<name>`. Any other name in a goal directory is left,
  logged `not a layout the sweep knows`.
- **What is swept.** An item's entries once the item is `merged`,
  `cancelled` or `superseded`; a `failed` item's once `now − statusAt`
  is over 7 days (`FailedWorkspaceKeep`; `WorkItem.StatusAt`); the
  whole `<goal>/` once the goal is `done` or `cancelled`; `<goal>/plan`
  once the goal is not `planning`; a kept session older than
  `harness.KeepSessionOutput`, now 14 days. Every other item status —
  `review`, `claimed`, `running`, `evaluating`, `held`, `ready`, … —
  keeps: a rework, a rebase or an answered question resumes in it.
- **What is never swept.** A directory whose sibling `.session`
  state's process (a sandboxed one's container) is alive, or whose
  subject is in `LiveSessions.IDs` — the loop's running and adopted
  item and goal ids — even when the platform calls its item merged; a
  finished goal's directory while any of its sessions is alive or its
  ids live. A slug no served org's goals have, or a seq the goal has no
  item for: `sweep: <path> left, unknown to the platform`. An org whose
  `Goals` errs, or a goal whose `GoalWorkItems` errs: `unread`, nothing
  of it judged (an outage is not a leak; a slug unknown while an org is
  unread is left as unread). A failed item with no `statusAt` (an older
  platform): kept, logged once per pass. Dot dirs (`.claims`,
  `.scratch`, `.locks`, `.plans`), `merge-batches`,
  `main-validations`, a `<project-slug>/research/…` tree and
  `LiveSessions.Keep` (the channel's `<node-slug>`): skipped silently.
  A symlink is Lstat'ed, logged `a symlink (not followed)` and left;
  nothing is removed through one, and a path that does not resolve
  under the root is left.
- **The log lines.** One per removal, `sweep: removed <goal>/<seq>
  (<why>, <bytes>)` — why the platform's answer (`item 3 merged`, `item
  4 failed 8d ago`, `goal done`, `goal running, not planning`, `kept
  15d, past 14d`), bytes the Lstat sizes under it, no symlink followed
  — then `sweep: removed <n> entries, <bytes> freed`. A removal that
  fails is logged `sweep: removing <goal>/<seq>: <err>; left for the
  next pass`. A removal is `RemoveScratch` (RemoveAll, then once more
  with every directory made writable), not `Git.RemoveWorkspace`: a
  removed worktree's `.git/worktrees/<n>` registration in the shared
  clone is left for `forgetStaleRegistrations` and the quartermaster's
  `pruneGoneWorktrees`, which forget a gone, unlocked one. The plan's `String()` is exactly those lines: a dry run is
  `PlanWorkSweep` without `ApplyWorkSweep`.
- **When.** Once at start-up, in `mesh node start`'s `adoptSessions`
  right after `SweepScopes` and before adoption, its live set the found
  sessions' item, goal and direction ids; then hourly from the loop —
  `Loop.WorkSweep` at the end of a tick once `Loop.WorkSweepEvery`
  (`DefaultWorkSweepEvery`, an hour) has passed on `Loop.Now` since the
  last, the first tick only starting the clock, so the first loop pass
  is an hour after the start-up one and never two run within the hour
  (`runWorkSweep`, `TestWorkSweepHourly`) — its live set
  `Loop.LiveSubjects()`: `RunningIDs`, the evaluations, plans, research,
  rebases and main validations running, and every `Adopted` subject.
  Both log through the node's logger and keep the channel's
  `<node-slug>`. A pass with nothing to remove logs nothing but what it
  left.
- **By hand.** `mesh node sweep` runs one pass on `~/.mesh/work` for
  every org the credential belongs to (`MyOrgs`), its live set every
  `.session` under the root whose state's pid is alive, whatever node
  it names (`node.AliveSessionSubjects`; a sandboxed one counts as
  alive), and prints the removal lines and the total to stdout, why an
  entry was left to stderr. `mesh node sweep --dry-run` prints the same
  lines, then `sweep: dry run; nothing removed`, and removes nothing
  (`TestNodeSweepDryRunPrintsAndRemovesNothing`).
- **A failed removal is retried.** It is logged `sweep: removing
  <name>: <err>; left for the next pass` and, its subject still finished,
  is in the next pass's plan again — the next hour's, or the next
  start's.

## Consequences

- Stopping the node process no longer ends a host session; the next
  node adopts it (ADR-009).
- A job's work dir gains a sibling while its session runs.
- Tests of a session's process run on the machine's real default unit
  (a transient scope where a user systemd answers).
