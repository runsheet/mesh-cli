---
title: Goal — A node.yaml the operator edits takes effect, and the node says so
kind: plan
status: running
parent: prd
summary: The mini's 04:00:22Z exit with status=2/INVALIDARGUMENT on 2026-09-29 was a nil-pointer panic in the evaluator — a failed Codex preflight turned to Claude, Credentials.Open's error overwrote acct with nil, and the deferred acct.Cleanup() dereferenced it — now fixed, and a panic anywhere in the node is one `mesh node: fatal:` line and exit 70.
---

# A node.yaml the operator edits takes effect, and the node says so

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
