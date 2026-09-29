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

### The session's end

Amended 2026-09-29 (same goal). Claude's own pid exiting is not the end
of what the session started, so **every end of a host session kills its
unit**: `ClaudeCode.Run` and `ClaudeCode.Attach` call `Unit.Kill` once
the leader has exited (or been stopped) and the output has been read to
its end — a clean exit, a non-zero exit, an `UnavailableError`, a
cancelled ctx, the job's `Timeout`, the hand-back's stop (the first run
and the resumed one, each its own unit) and an adopted session's end
alike. The stops a cancel, a timeout or the hand-back make *are* that
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
- **The node says it**: each session's tail — `afterBuild`,
  `afterEvaluate`, the planner's `readPlan`, the director's `handIn`,
  `afterRebase` — logs each session's non-empty `Killed` as one line
  (a handed-back build's two runs are two lines, and its note both),
  `item <label>: session end killed <n> process(es) the agent left
  running: <pid comm, pid comm>` (`KilledNote`; `goal <slug>:` for a
  plan, `<project>/<direction>:` for research), and a `KillError` as
  `item <label>: kill: <err>`. A build or a rebase sends the same
  sentence as a progress note on the item, as a `tools: …` note is
  sent; an evaluation adds it to its verdict's notes. The platform
  takes no progress note on an item in evaluating, so an evaluation that
  records no verdict — a timeout, an error, no verdict, no context —
  holds its sentence (`Evaluator.LeftRunning`, per item, for the node
  process's life) and the item's next verdict's notes carry it.
- **An adopted session no longer the node's**: `FoundSession.Kill`
  replaces the stop that ran only while claude was alive — an item,
  plan or direction the platform no longer has on this node has its
  unit killed whether or not claude still runs (what the agent left
  there may outlive it), and what it killed is logged the same way.
- Sandboxed and Codex sessions are not in a unit and report nothing.

## Consequences

- Stopping the node process no longer ends a host session; the next
  node adopts it (ADR-009).
- A job's work dir gains a sibling while its session runs.
- Tests of a session's process run on the machine's real default unit
  (a transient scope where a user systemd answers).
