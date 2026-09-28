---
title: Goal — A node upgrade survives a slow platform: the confirm window retries and says why, and a new process logs before it can block
kind: plan
status: running
parent: prd
summary: A pending update retries registration and heartbeats for node.yaml update.confirm_within (default 5 min) and logs each failure, a slow platform is no longer "did not start", node.log's first lines come before any platform call, and the console shows updateError once runsheet's Node record carries it.
---

# Goal — A node upgrade survives a slow platform: the confirm window retries and says why, and a new process logs before it can block

Mesh goal `upgrades-survive-a-slow-platform`, running: items 1–3 merged
(2026-09-28), item 4 (the mesh-specs amendment) in review, this document
item 5.

## Statement

2026-09-28 05:53Z on the mini: `mesh node upgrade --binary` to 0a04088
restarted the service. Three times, the new process exited with status 1
after exactly 60 s, with not one line in node.log, and the node rolled back
to 72ff910 (update.json: `'0a04088 did not start'`). Run by hand at 06:51Z,
the same binary started in seven seconds.

The difference was the platform. The mini's load was 200 and every gateway
call timed out (`context deadline exceeded` at 05:53:20). The cause was
`nodeStart`'s 60 s registration context: registration could not finish
inside it, so the process exited before its first heartbeat could confirm
the update (`Confirm` in `internal/node/selfupdate.go`). It exited before it
had logged anything, and the start count read three such exits as "did not
start". So a platform blip or a busy box rolled every upgrade back, and the
node never said why.

The fix:

1. The confirm window is generous and retried. It defaults to 5 min and is
   set by node.yaml `update.confirm_within`. Each refused or timed-out
   attempt is logged with its error, and a timeout is logged as the
   platform being slow. Only a failure to *run* counts as "did not start".
2. node.log gets the new process's first lines before anything that can
   block.
3. update.json names the real cause, and the console shows it.
4. `mesh node upgrade` prints the window and the log to watch.

## Acceptance

- **mesh-cli tests.**
  - A new process whose heartbeats time out for two minutes and then
    succeed confirms the update, with no rollback.
  - One whose binary exits at once is rolled back, and the exit is named.
  - A process's first log lines name its version and the pending update
    before any platform call.
- **Live.** An upgrade on the mini during a CI burst confirms rather than
  rolling back, or update.json names the platform timeout.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | Node: update.json names why a start or a confirm failed, node.yaml gets update.confirm_within, and node.log's first lines name the version, config and pending update | merged | mesh/upgrades-survive-a-slow-platform-1 | runsheet/mesh-cli#275 (a9f7ff9) | $3.49 |
| 2 | Node: the confirm window — a pending update retries registration and heartbeats for update.confirm_within, logs each failure with its error, and only a run failure or an elapsed window ends the process; mesh node upgrade prints the window and the log path | merged | mesh/upgrades-survive-a-slow-platform-2 | runsheet/mesh-cli#277 (00f4b9b) | $2.26 |
| 3 | Console: the Nodes list and node page show a node's updateError when the node record carries it | merged | mesh/upgrades-survive-a-slow-platform-3 | runsheet/mesh-portal#14 (54476c1) | $0.70 |
| 4 | Design: goal-node-upgrades-keep-sessions, ADR 008 and the TRD's self-update section describe the confirm window, what counts as did not start, the first log lines and updateError naming the cause | review | mesh/upgrades-survive-a-slow-platform-4 | runsheet/mesh-specs#65 | $1.13 |
| 5 | Goal doc: upgrades-survive-a-slow-platform — what landed, the mini's by-hand upgrade during a CI burst, and the platform half owed | running | mesh/upgrades-survive-a-slow-platform-5 | — | — |

### What each landed

**Item 1: the state fields and the first log lines** (#275).

- **update.json's `State`** (`internal/selfupdate/state.go`) gains two
  fields. Neither affects `Pending()`, and `Confirm` clears both.
  - `startError` (`NoteStartFailure`) is why the last start of the pending
    update ended before it could confirm: a config it could not read, a
    panic or an exited process.
  - `confirmError` (`NoteConfirmError`) is the last error a heartbeat hit
    while the update was unconfirmed: a refusal or a timeout.
- **`Rollback` names the real cause.** It takes the confirm window and
  writes one of these reasons into `lastError`, in this order:
  - `'<to> did not start: <startError>'`
  - `'<to> did not confirm within <window>: <confirmError>'`
  - `'<to> did not start; rolled back to <from>'`, when neither field is
    set.
- **`registerNode`** (`internal/commands/nodecmd.go`) holds everything a
  start needs before the loop: `LoadConfig`, the accounts' secrets,
  `platformClient`, the work root and registration. With an update
  pending, an error or a panic there is recorded with `NoteStartFailure`,
  and a panic is re-raised after it is recorded.
- **The first log lines.** `node.StartLines` runs right after
  `startUpdates` and before `LoadConfig` or any platform call. It logs the
  version, the pid and the config path, then one of:
  - `'confirming update <from> → <to>: start <n> of 3, rolls back on the third'`
  - `'no update pending'`
- **node.yaml `update.confirm_within`** (`Config.ConfirmWithin()`) sets the
  window. It defaults to `selfupdate.DefaultConfirmWithin`, 5 minutes.

**Item 2: the confirm window, `ErrUnconfirmed` and the upgrade's printed
window** (#277).

- **The window** opens at process start (`Loop.Started`). While an update
  is pending (`Loop.Confirming`), `registerNode` retries a failed
  registration (60 s per attempt) on the heartbeat interval, and the loop
  keeps heartbeating.
- **Each failure** is logged and recorded with `NoteConfirmError`:
  `'self-update: confirming <to>: heartbeat <org>: <err>; retrying for <left> more'`.
  A timeout adds `(the platform is slow)` (`PlatformSlow`).
- **The first heartbeat through** confirms the update.
- **A window that elapses with none through** is `ErrUnconfirmed`
  (`*UnconfirmedError`, naming the last error). `endRun` logs
  `'self-update: <to> did not confirm within <window>: <err>; exiting for the service to start it again'`
  and the process exits 1. The third such start rolls back, naming that
  error.
- **`mesh node upgrade`** (`internal/commands/nodeupgrade.go`) prints the
  window and `~/.mesh/node.log` after the restart.
- **Tests** cover the goal's three clauses: two minutes of heartbeat
  timeouts and then a success confirms, an immediate exit rolls back with
  the exit named, and the first lines come before any platform call.

**Item 3: the console's updateError** (runsheet/mesh-portal#14).

- `src/lib/meshApi.ts`: `NodeRow` gains an optional
  `updateError?: string | null`.
- `src/pages/Nodes.tsx`: an `UpdateErrorBadge` ("update failed", with the
  reason as its `title`) appears only when `updateError` is non-empty.
- `src/pages/NodeDetail.tsx`: the full text appears under the Version card,
  only when present, with tests in `NodeDetail.test.tsx`.
- An absent or empty field shows nothing.

## Design

The self-update amendment for this goal is in
`docs/adr/adr-002-signed-tag-releases.md` ("Amendment: update.json names
why a start or a confirm failed"). The mesh-specs side is item 4: the goal
`goal-node-upgrades-keep-sessions`, ADR 008 and the TRD's self-update
section (runsheet/mesh-specs#65). The console display follows runsheet/mesh
ADR-004, `update-error-on-the-console`.

## node.yaml: update.confirm_within

```yaml
update:
  confirm_within: 5m   # how long a new process keeps trying to confirm; default 5m
```

This is how long a new process keeps trying to confirm a pending update,
retrying registration and heartbeats, before it exits 1. On a box that is
often loaded, raise it. With no update pending, it does nothing.

## Live check

The operator runs this on the mini while CI is busy, with the linux/amd64
binary copied to `~/bin/mesh.new`:

```sh
mesh node upgrade --binary ~/bin/mesh.new
```

It prints the following, from `nodeupgrade.go`:

```
/home/chris/bin/mesh.new answers `version` with <version>.
<version> is in place at /home/chris/bin/mesh; the previous binary is kept at /home/chris/bin/mesh.prev.
Restarted io.projectmesh.mesh-node.service: the new process adopts the sessions the old one left running (`systemctl --user list-units 'mesh-session-*'`).
The new process confirms the upgrade with its first heartbeat, within 5m0s (node.yaml update.confirm_within); watch ~/.mesh/node.log. A binary that cannot run, or cannot confirm inside the window on 3 starts, is rolled back to /home/chris/bin/mesh.prev and ~/.mesh/update.json says why.
```

The shell expands `~` in the argument; the unit is `node.ServiceLabel`, and `5m0s` is the window as Go prints it.

Then watch the log:

```sh
tail -f ~/.mesh/node.log
```

The first lines name the version, the pid, the config and
`confirming update <from> → <to>: start 1 of 3, rolls back on the third`.
Next come any `self-update: confirming <to>: heartbeat …; retrying for …`
lines, with `(the platform is slow)` on a timeout, and then the confirm.

Once it settles, read the state:

```sh
cat ~/.mesh/update.json
```

It passes if one of these holds:

- `confirmedAt` is set, meaning the upgrade survived the burst.
- The upgrade rolled back and `lastError` reads `'<to> did not confirm within 5m0s: … context deadline exceeded …'`,
  which names the platform timeout rather than "did not start".

Until the platform half below lands, the console's Nodes page cannot show
that `lastError`. Read it from update.json.

## Owed to the platform

**runsheet: the Node record's `updateError` field.**

- **Already done:**
  - The node sends `updateError` on its heartbeat (`gateway.HeartbeatExtras`,
    only when set).
  - The console renders it (mesh-portal#14).
- **Owed:** runsheet must accept `updateError` on the heartbeat, keep it
  on the Node record, and serve it on the node query the console reads. It
  must also clear it when a later heartbeat sends none.
- **Until then:** the console's badge and detail stay dark, and the cause
  of a rollback is only in `~/.mesh/update.json` and `~/.mesh/node.log` on
  the machine.
- **Order:** per the rule that the platform side lands first, runsheet's
  part goes in before anything else relies on the field.

## After merge: the mini

Deploy by hand, as the README's "Upgrading a node without ending sessions"
describes:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o mesh-linux ./cmd/mesh
scp mesh-linux mini:~/bin/mesh.new
ssh mini mesh node upgrade --binary ~/bin/mesh.new
```

Then run the Live check above.

## Log

- 2026-09-28 05:53Z · the mini rolls 0a04088 back to 72ff910 during a load-200 burst; nothing in node.log
- 2026-09-28 06:51Z · the same binary starts by hand in seven seconds
- 2026-09-28 · items 1 (#275), 2 (#277) and 3 (mesh-portal#14) merged; item 4 (mesh-specs#65) in review; goal doc written
