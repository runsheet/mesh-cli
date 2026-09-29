---
title: Goal — A node.yaml the operator edits takes effect, and the node says so
kind: plan
status: running
parent: prd
summary: The mini's capacity 6 → 8 did take effect in the loop at 06:17:09Z (logged) but never reached the platform, because a fixed node on one account heartbeated no capacity and only registration sets NodeIndex.capacity; a fixed node now heartbeats `scaling {capacity, min, max, reason: fixed}` (mesh-cli ADR-018). Separately, the mini's 04:00:22Z exit with status=2/INVALIDARGUMENT on 2026-09-29 was a nil-pointer panic in the evaluator — a failed Codex preflight turned to Claude, Credentials.Open's error overwrote acct with nil, and the deferred acct.Cleanup() dereferenced it — now fixed, and a panic anywhere in the node is one `mesh node: fatal:` line and exit 70.
---

# Goal — A node.yaml the operator edits takes effect, and the node says so

Mesh goal `node-yaml-takes-effect-or-says-why-not`, running. Item 1 (this
section) found and fixed why the capacity change never reached the
platform. Items 2–4 cover every reloadable field, the 04:00:22Z
`status=2/INVALIDARGUMENT` exit (below, in The 04:00:22Z exit), and the
unbounded node.log.

## Statement

On 2026-09-29 the mini's `~/.mesh/node.yaml` said `capacity: 8` (mtime
11:16:18Z) under a node started 04:00:32Z on v0.2.188. At 13:30Z the
platform's `NodeIndex(mini)` still said `capacity: 6`, and a grep of
node.log after 11:16 found no `capacity 6 → 8` line and no
`reload config:` line.

## Root cause

**Possibility (d) held: the loop applied the new capacity and logged it,
but the platform was never told.** (a), (b) and (c) are ruled out by the
evidence below. The operator edited node.yaml at 06:15–06:17Z, not at
11:16Z. The 11:16 edit only added a `codex:` block, so the search for the
line began five hours after the line was written.

### Evidence from the mini

Commands run on the mini on 2026-09-29 at about 21:45Z. Log lines are
from `~/.mesh/node.log` (372 MB), found by timestamp prefix.

**The service reads the file the operator edited (rules out b).**

```
$ systemctl --user cat io.projectmesh.mesh-node | grep -E 'ExecStart|Environment'
ExecStart=/home/chris/bin/mesh node start --mode=poll --unattended
Environment=PATH=/usr/bin:/home/chris/bin:/home/chris/.local/bin:/opt/homebrew/bin:/usr/local/bin:/bin:/usr/sbin:/sbin
Environment=HOME=/home/chris
$ tr '\0' '\n' < /proc/$(cat ~/.mesh/node.pid)/environ | grep ^HOME
HOME=/home/chris
$ grep -n '2026/09/29 04:00:32 mesh node' ~/.mesh/node.log
5921770:mesh node: 2026/09/29 04:00:32 mesh node v0.2.188, pid 3303985, config /home/chris/.mesh/node.yaml
```

**The capacity changed at 06:15–06:17, not at 11:16.** The operator's
own backups, each taken just before an edit:

```
$ stat -c '%y' ~/.mesh/node.yaml.bak-20260929-061520 ~/.mesh/node.yaml.bak-20260929-111617
2026-09-29 06:15:20.510041847 +0000      # capacity: 6
2026-09-29 11:16:17.962121594 +0000      # capacity: 8 — already
$ diff ~/.mesh/node.yaml.bak-20260929-111617 ~/.mesh/node.yaml
10a11,21
> # The operator's own ChatGPT login, so evaluations prefer Codex: …
> codex:
>   binary: /home/chris/.local/bin/codex
>   accounts:
>     - name: chatgpt-pro
>       home: ~/.codex
```

**The reload ran on this mode and applied it, logged at that moment
(rules out a and c).** It was the same process: the service's next start
was 14:13:19Z.

```
$ grep -n 'capacity' ~/.mesh/node.log | grep '2026/09/29 0[4-9]\|2026/09/29 1[0-3]'
6015200:mesh node: 2026/09/29 06:15:46 capacity 6 → 6–8
6015202:mesh node: 2026/09/29 06:15:46 capacity 6–8: scaling by what its 2 account(s) afford from 6
6015207:mesh node: 2026/09/29 06:17:09 capacity 6 → 8
$ journalctl --user -u io.projectmesh.mesh-node --since '2026-09-29 00:00' | grep -E 'Started|status='
Sep 29 04:00:22 … Main process exited, code=exited, status=2/INVALIDARGUMENT
Sep 29 04:00:32 … Started io.projectmesh.mesh-node.service …
Sep 29 14:13:19 … Started io.projectmesh.mesh-node.service …
```

The operator briefly wrote a range (`{min: 6, max: 8}`) and then a plain
`8`, and each was in force on the next 30 s tick. The loop then ran at 8.
When paced it caps new work at half its capacity, and that half went from
3 to 4:

```
03:17:20 limits: paced … at most 3 item(s) at once, and no research     # capacity 6
06:32:15 limits: paced … at most 4 item(s) at once, and no research     # capacity 8
```

**The platform never heard (d).** `NodeIndex.capacity` is set by
`registerNode` alone (`Capacity: cfg.Capacity.Max`,
`internal/commands/nodecmd.go`), and only a start registers. The
heartbeat's `scaling` sets `NodeIndex.scaling_capacity` (runsheet
`platform/aggregates/node.go`, `scalingSeen`), but
`Loop.heartbeatScaling` returned nil for a fixed capacity on fewer than
two accounts. From 06:15:46 to 06:17:09 the range sent
`scaling.capacity 6`; from 06:17:09 a plain `8` sent nothing. So:

```
13:30:02Z  NodeIndex(mini)  capacity: 6, running: 4, version v0.2.188          (the goal's reading)
14:24:10Z  mesh node v0.2.188, pid 786507 … poll mode: … capacity 8             (restart re-registers)
21:45:14Z  mesh_health_nodes(runsheet) → mini: "capacity": 8, "version": "v0.2.191"
```

It was 8 only after the 14:13–14:25 restarts re-registered the node.

### The test, failing before the fix

`TestCapacityReloadFromNodeYAMLReachesTheHeartbeat`
(`internal/node/capacity_reload_test.go`) drives a `Loop` whose `Reload`
is `Reloader` over a node.yaml on disk. The file goes from `capacity: 6`
to `capacity: 8` between two Ticks. The test asserts `Capacity() == 8`,
the whole log line `capacity 6 → 8`, and `scaling.capacity == 8` on the
heartbeat the fake gateway received after the second tick. It was
committed alone on top of main (882ceab), with main's non-test files:

```
$ go test ./internal/node/ -run TestCapacityReloadFromNodeYAMLReachesTheHeartbeat -count=1 -v
=== RUN   TestCapacityReloadFromNodeYAMLReachesTheHeartbeat
    capacity_reload_test.go:42: the heartbeat after the reload carries scaling <nil>, want capacity 8
--- FAIL: TestCapacityReloadFromNodeYAMLReachesTheHeartbeat (0.01s)
FAIL
FAIL	projectmesh.io/mesh-cli/internal/node	0.016s
```

It fails at the heartbeat, not at `Capacity()` or the log line, which
matches what the mini showed.

### The fix

`Loop.heartbeatScaling` now sends `scaling` for a fixed node on any
number of accounts, the operator's own login included, as
`{capacity: N, min: N, max: N, reason: fixed, accounts: […]}`. It rides
the existing gate: only when the gateway's `HeartbeatNodeInput` takes
`scaling`, introspected and cached for an hour. `reason: fixed` is
already sent for a fixed node on several accounts, so the platform
accepts it unchanged. The console shows `scaling.capacity` when present
(runsheet/mesh ADR-001), so a node.yaml capacity change shows there
within one heartbeat. The contract change is recorded in
`docs/adr/adr-018-a-fixed-node-heartbeats-its-capacity.md` and AGENTS.md's
heartbeat-extras paragraph.

Tests:

- `TestCapacityReloadFromNodeYAMLReachesTheHeartbeat` passes.
- `TestFixedSingleAccountHeartbeatBody` asserts the whole heartbeat body
  of a fixed single-account node:
  `{"namespace":"runsheet","running":0,"scaling":{"accounts":[],"capacity":3,"max":3,"min":3,"reason":"fixed"},"version":"v0.2.203"}`.
  It also checks that a schema without `scaling` gets none.
- `TestFixedNodeHeartbeatsItsCapacityOnAnyNumberOfAccounts` and
  `TestScalingHeartbeat` now expect the fixed `scaling`.

The `capacity %d → %d` line in `setRange` needed no change: it already
logs whenever the fixed capacity in force changes, as 06:17:09 shows. The
new test asserts it as a whole line.

Not changed: what capacity means, the scaler, and a ranged node's
behaviour (ADR 004).

### Still owed

- **Platform:** `NodeIndex.capacity`, which `mesh_health_nodes` reports,
  is still the registered value until the next start. runsheet should
  read `scaling_capacity` when set, or keep `capacity` in step with it.
- **After the release reaches the mini:** the goal still needs
  `NodeIndex` before and after a live node.yaml edit, with the file's
  mtime and the log line. The fix is not yet on the mini (v0.2.191).
- `Reloader`'s `register` callback registers a newly served org with
  the capacity the node started with (`registerIn(ctx, gw, cfg, …)` on
  the start-time `cfg`). The heartbeat's `scaling` now corrects it on
  the next tick.

## The 04:00:22Z exit

### What the machine recorded

The journal of the service (`journalctl --user -u
io.projectmesh.mesh-node.service`, 2026-09-29, the mini's clock is UTC):

```
2026-09-29T04:00:22+00:00 mini systemd[1153]: io.projectmesh.mesh-node.service: Main process exited, code=exited, status=2/INVALIDARGUMENT
2026-09-29T04:00:22+00:00 mini systemd[1153]: io.projectmesh.mesh-node.service: Failed with result 'exit-code'.
2026-09-29T04:00:22+00:00 mini systemd[1153]: io.projectmesh.mesh-node.service: Consumed 20min 22.917s CPU time.
2026-09-29T04:00:32+00:00 mini systemd[1153]: io.projectmesh.mesh-node.service: Scheduled restart job, restart counter is at 1.
2026-09-29T04:00:32+00:00 mini systemd[1153]: Started io.projectmesh.mesh-node.service - Mesh node: runs work items of goals on this machine.
```

The unit appends stderr to `~/.mesh/node.log` (`StandardError=append:`),
so the runtime's own trace is there. Found by timestamp, not by a
whole-file grep (the file was 372 MB by then): the last line of 03:5x is
line 5913294, the first of 04:01 line 5921825, and within that range:

```
mesh node: 2026/09/29 04:00:08 orgs foghornid, , wardroomid, go-apis, chriskolenko, latchkeyid, projectmesh-io, triplineid, purserid, runsheet → triplineid, latchkeyid, projectmesh-io, runsheet, , chriskolenko, foghornid, go-apis, wardroomid, purserid
mesh node: 2026/09/29 04:00:12 item integrator-lives-within-the-rate-limit/25: context unavailable: fetch adr-020-dependents-start-on-evaluated-work: context: 429 Too Many Requests: Rate exceeded.
mesh node: 2026/09/29 04:00:20 warning: live goals and items unavailable, INDEX.md lists documents only: items of a-process-sends-only-what-it-declares: gateway: 429 Too Many Requests: Rate exceeded.
mesh node: 2026/09/29 04:00:20 item integrator-lives-within-the-rate-limit/25: evaluate budget $3.00 (the session's own cap)
mesh node: 2026/09/29 04:00:22 item integrator-lives-within-the-rate-limit/25: Codex preflight failed (initialize: 429 Too Many Requests: Rate exceeded.); evaluating on Claude
panic: runtime error: invalid memory address or nil pointer dereference
[signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0xcda6d5]

goroutine 53597 [running]:
projectmesh.io/mesh-cli/internal/node.(*Evaluator).Evaluate.func2()
	projectmesh.io/mesh-cli/internal/node/evaluate.go:262 +0x15
projectmesh.io/mesh-cli/internal/node.unlessDetached({0x11160b0?, 0x309e4aa2c390?}, 0x309e4ad3f750)
	projectmesh.io/mesh-cli/internal/node/adopt.go:348 +0x35
projectmesh.io/mesh-cli/internal/node.(*Evaluator).Evaluate(_, {_, _}, {{0x309e4ae06570, 0x24}, {0x309e4ae06540, 0x24}, {0x309e4ae065d0, 0x24}, 0x19, ...})
	projectmesh.io/mesh-cli/internal/node/evaluate.go:361 +0x2cab
projectmesh.io/mesh-cli/internal/commands.runPollMode.evaluateWith.func3(...)
	projectmesh.io/mesh-cli/internal/commands/nodecmd.go:1176 +0x145
projectmesh.io/mesh-cli/internal/node.(*Loop).evaluateHeld.func1(...)
	projectmesh.io/mesh-cli/internal/node/loop.go:1115 +0x11a
created by projectmesh.io/mesh-cli/internal/node.(*Loop).evaluateHeld in goroutine 1
	projectmesh.io/mesh-cli/internal/node/loop.go:1107 +0x92f
mesh node: 2026/09/29 04:00:32 self-update: the update to v0.1.0 failed: release v0.1.0 has no SHA256SUMS on runsheet/mesh (404: not published there, or not a signed release)
mesh node: 2026/09/29 04:00:32 mesh node v0.2.188, pid 3303985, config /home/chris/.mesh/node.yaml
```

### The cause

Not an argument: status 2 is the Go runtime's exit for an unrecovered
panic, and `INVALIDARGUMENT` is only systemd's LSB name for the number.
Ruled out with the trace in hand: a cobra error (`cmd/mesh/main.go`
exits 1), `log.Fatal` (exits 1) and `flag`'s ExitOnError on an exec'd
restart (`execSelf`) — no restart was logged, and the process had run
six hours.

The panic is in `Evaluator.Evaluate` (v0.2.188, `evaluate.go:262`; 275
on today's main). The evaluation opens its first account on the family
with headroom — Codex — and defers `unlessDetached(ctx, func() {
acct.Cleanup() })`, a closure over the variable `acct`. The Codex
preflight failed on a 429 from the gateway, so the unpinned evaluation
turned to Claude:

```go
if acct, err = e.Credentials.Open(ctx, ref, sc.NodeID, e.Accounts, AdapterClaudeCode); err != nil {
	return err
}
```

The claim-time read was rate-limited the same second, `Open` returned
`(nil, err)`, the assignment overwrote `acct` with nil, and the return
ran the deferred closure: `(*SessionAccount)(nil).Cleanup()` reaches
the embedded `*AccountSession` through a nil pointer. `AccountSession.Cleanup`
is nil-safe; the promotion through a nil `*SessionAccount` is not. The
goroutine was `evaluateHeld`'s, so nothing recovered it and the whole
node died — its four build and plan sessions ran on (`Job.Outlive`) and
the 04:00:32 process adopted them.

### The fix

`Open`'s result lands in its own variable and replaces `acct` only on
success (`internal/node/evaluate.go`), so the error comes back to the
loop as an ordinary evaluation error and the Codex account already
cleaned up is not touched again. The other `Credentials.Open` call sites
(`runner.go`, `plan.go`, `research.go`, `rebase.go`, the second opinion)
declare their account fresh and return before deferring its cleanup;
none reassigns a deferred-over account.

The test that reproduces it is
`TestCodexPreflightFailureWithNoClaudeAccountReturnsTheError`
(`internal/node/evaluate_family_test.go`): a Codex preflight that fails
with the mini's `initialize: 429 Too Many Requests: Rate exceeded.` and a
Claude account that cannot be opened (none issued). **On main it panics**
with the mini's own frames — `(*Evaluator).Evaluate.func2()` →
`unlessDetached` → `Evaluate` — before the fix, and passes after it,
returning `ErrNoAccount`.

### A death that names itself

A recover on the start path alone would not have caught this panic: it
ran on a goroutine the loop started. `internal/crash`'s `Guard` is
deferred at the top of `mesh node start` (the cobra `RunE`) and of every
goroutine the node starts — the loop's build, evaluate, plan, research
and rebase goroutines, adoption's, the build and rebase heartbeat
tickers, the self-update, the drain-signal watcher and channel mode's
goal watcher and session. A panic there writes one line and the stack
to node.log and exits 70 (`crash.ExitPanic`, sysexits' EX_SOFTWARE,
which systemd shows as `status=70/SOFTWARE`):

```
mesh node: fatal: runtime error: invalid memory address or nil pointer dereference (version v0.2.xxx)
goroutine 53597 [running]:
…
```

The unit's `Restart=always` and the LaunchAgent's `KeepAlive` restart on
it as on any exit; neither maps exit statuses, so nothing changes there.
The guard's tests assert the whole line and the status:
`TestNodeStartPanicNamesItself` (`internal/commands`, a real child
process running `mesh node start --mode=poll --unattended` whose start
path panics: exit 70, not 2, and the first line exactly `mesh node:
fatal: … (version <v>)`; without the guard it exits 2),
`TestPanickingEvaluationNamesItself` (`internal/node`, the goroutine
this panic ran on) and `internal/crash`'s own. Goroutines below the node
— the harness's stream readers, the sandbox's sidecar probes, the
session unit's waiter — are not guarded; a panic there still exits 2 with
the runtime's trace in node.log, found as above by `^panic:`.
