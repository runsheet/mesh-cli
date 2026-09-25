---
summary: How mesh-cli starts implementing mesh-specs ADR 008 — internal/sessionunit runs a host claude session in a unit of its own (a transient `systemd-run --user --scope`, else a detached setsid process group; the sandbox's pod is the unit later), its stream-json in `<workdir>.session/output.jsonl`, its stderr in `stderr.log`, and a secret-free `state.json` (node, item/goal/direction, claim, kind, model, account, pid, unit, startedAt, offset) written atomically, read back by a Follower that tails the file line by line; ClaudeCode.Run keeps its result and error semantics, and a cancelled context or timeout stops the unit.
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
  of their own; `Stop` sends SIGTERM to the group and SIGKILL after
  `StopGrace` (5s), and the scope is killed through `systemctl --user
  kill` if anything of it is left. The sandbox's pod (ADR 007) is the
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
  `state.json` when the session id is first seen and then at most once
  a second, so a later node can resume from a line boundary.
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

## Consequences

- Stopping the node process no longer ends a host session; until the
  adoption lands, such a session runs to its end unread, and its item
  is re-run as before.
- A job's work dir gains a sibling while its session runs.
- Tests of a session's process run on the machine's real default unit
  (a transient scope where a user systemd answers).
