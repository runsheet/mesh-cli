---
title: Goal — A node.yaml the operator edits takes effect, and the node says so
kind: plan
status: done
parent: prd
summary: The mini's capacity 6 → 8 took effect in the loop at 06:17:09Z but never reached the platform because a fixed node on one account heartbeated no capacity and only registration sets NodeIndex.capacity (fixed: a fixed node now heartbeats `scaling`, mesh-cli ADR-018); the same six hours' 04:00:22Z exit, status=2/INVALIDARGUMENT, was an unrecovered nil-pointer panic in the evaluator's Codex-to-Claude fallback, now impossible anywhere in the node (`crash.Guard`, exit 70); and `~/.mesh/node.log` — 339 MB and never rotated — is now capped at 10 MB × 5 files with session transcripts in their own file.
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
- **After the release reaches the mini:** see `## The mini before and
  after`, below — as of this item the mini is still on v0.2.191, so the
  reading there is the version and the release that carries the fix,
  not yet a live before/after on the fixed code.
- `Reloader`'s `register` callback registers a newly served org with
  the capacity the node started with (`registerIn(ctx, gw, cfg, …)` on
  the start-time `cfg`). The heartbeat's `scaling` now corrects it on
  the next tick.

The mini's `capacity` was raised from 6 to 8 in `~/.mesh/node.yaml` and
never took effect, and `~/.mesh/node.log` — 339 MB and never rotated —
was too large to show it. This document collects what each item of the
goal found and landed.

## The mini before and after

**Before the fix.** `NodeIndex(mini)` from the platform, 2026-09-29
13:30:02Z (Root cause, above): `capacity: 6, running: 4, lastSeenAt
13:30:02Z, version v0.2.188`. **`stat -c %y ~/.mesh/node.yaml` on the
mini itself is not obtainable this session**: this session has no
shell on the mini (`ssh mini` refuses this session's key, tried again
while writing this section, and confirmed once more below in `##
node.log`), and no earlier session of this goal ran `stat` on the live
file either — only on the operator's own pre-edit backups (Root cause,
above). The closest evidence is those backups, each timestamped just
before an edit: `node.yaml.bak-20260929-061520` at `2026-09-29
06:15:20.510041847 +0000` still holds `capacity: 6`, and the loop
logged `capacity 6 → 8` at 06:17:09 — about 109 seconds later — so the
live file's own mtime at that moment sits somewhere in that window, not
at the 06:15:20 the backup shows (which is the moment *before* the
edit, taken by whatever made the backup, not the edited file's own
mtime). The whole node.log line the loop wrote the moment it applied
`capacity: 8`:

```
mesh node: 2026/09/29 06:17:09 capacity 6 → 8
```

A live `stat -c %y ~/.mesh/node.yaml` reading is owed from a session
with shell access to the mini.

**After: the mini has not yet run a release carrying the fix.** Queried
live this session, `mesh_health_nodes(org: runsheet)`, 2026-09-29
23:39:53Z:

```
mini: version v0.2.191, desired_version v0.2.191, capacity: 8,
running: 4, last_seen_at: 2026-09-29T23:39:53Z,
commits_behind_main: 21, oldest_unrun_merged_at: 2026-09-29T14:28:15Z
```

The mini is still on **v0.2.191** (`8eba662`, PR #434 — registration
declares an owner), the tag it was already running at 13:30Z, and is 21
commits behind `main`'s head (`38f854d`). It has picked up none of
items 3, 6, 7, 8 or 9 (checked by `git merge-base --is-ancestor
<commit> v0.2.191`, all five "no"). By the same check against each
release tag: item 3's panic guard (`348f147`) first lands in
**v0.2.203** (tagged 2026-09-29 22:09:18Z); item 6's capacity-heartbeat
fix (`634e2e6`) first in **v0.2.204** (22:25:22Z); item 7's log
rotation (`f4b9cbb`) first in **v0.2.205** (22:48:13Z); item 9's reload
failure logging (`54ffce3`) first in **v0.2.206** (23:05:40Z); item 8's
every-field reload logging (`38f854d`) first in **v0.2.208**
(23:24:32Z), the latest release, which carries all five.

The `capacity: 8` the platform shows now, **labelled version v0.2.191**,
is not evidence of the fix: it is the 14:13–14:25Z restart's
re-registration the Root cause section already used to show the
platform only reached 8 by restarting (`registerNode` sets
`NodeIndex.capacity` from `cfg.Capacity.Max` at start, unrelated to this
goal's change). Proof that a live edit reaches the platform *without* a
restart is `TestCapacityReloadFromNodeYAMLReachesTheHeartbeat`, above;
the mini's own before/after on the fixed code is owed once it runs
v0.2.204 or later. Per this item's instruction, this session did not
restart or reinstall the mini's node to make that happen.

## What a reload logs

Every reloadable field logs one whole line with its old and new value
the tick it changes, and nothing when the file is unchanged
(`TestReloadUnchangedLogsNothing`, `internal/node/reload_fields_test.go`).
A test per field rewrites `~/.mesh/node.yaml` under a temp `HOME`
between two ticks and asserts the value through the loop's own accessor
and the whole line:

| Field | Log line (example) | Test |
|---|---|---|
| capacity | `capacity 6 → 8` (fixed); `capacity 3 → 2: clamped into 1–2` (ranged) | `TestCapacityReloadFromNodeYAMLReachesTheHeartbeat` (`internal/node/capacity_reload_test.go`); `TestScalingReloadClampsIntoTheNewRange` (`internal/node/scaling_loop_test.go`) |
| limits | `limits: pace five-hour 70% → 80%, stop 90% → 95%` | `TestReloadLimitsFromDisk` |
| accounts | `account sub-a: token_file ~/a.token → ~/a2.token`; membership as `accounts sub-a, api-b → api-b, api-c`; a kind change as `account api-b: kind api → subscription, token_file (none) → ~/b.token, api_key_env KEY_B → (none), daily_cap $50.00 → none, monthly_cap $1000.00 → none` | `TestReloadAccountsFromDisk` |
| budgets | `budget build/sonnet $4.00 → $7.00`; `project budget mesh none → $20.00` | `TestReloadBudgetsFromDisk` |
| context | `context: bytes_max 245760 → 131072, doc_bytes_max 61440 → 32768` | `TestReloadContextFromDisk` |
| claim | `claim: unmerged_per_repo 0 → 2` | `TestReloadClaimFromDisk` |
| orgs | `orgs acme → acme, globex` | `TestReloadOrgsFromDisk` |

All seven in `internal/node/reload_fields_test.go` unless noted.

**A reload that cannot be applied** logs one physical line per tick and
keeps the last good values in force (`internal/node/reload_fail_test.go`,
item 9): a bad `node.yaml` —

```
reload config: node.yaml (keeping the last good capacity and limits): parse <path>: yaml: unmarshal errors:; line 5: cannot unmarshal !!str `high` into float64; line 7: cannot unmarshal !!str `lots` into int
```

— asserted whole by `TestReloadFailUnparseableNodeYAMLKeepsTheLastGood`
(a YAML error's own newlines folded into the one line); the same shape
covers a failing `MyOrgs` alone (`reload config: my orgs: gateway
unavailable`, `TestReloadFailMyOrgsKeepsTheOrgsAndAppliesNodeYAML` — the
orgs are kept but node.yaml's other fields still apply, since the
operator's edit does not wait on the gateway), both causes joined on one
line (`TestReloadFailBothInOneTickIsOneLine`), no org left to serve
(`reload config: no org left to serve (keeping the orgs as they were)`,
`TestReloadFailNoOrgLeftKeepsTheOrgs`), and one line per failing tick,
none once a tick's reload succeeds again
(`TestReloadFailLogsOneLinePerTick`).

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
the runtime's trace on stderr — node.log under a unit installed before
item 4, the journal after — found as above by `^panic:`.

## node.log

Item 4 (ADR-019, `internal/node/logfile.go`).

**The limit.** `mesh node start` opens `~/.mesh/node.log` itself and
rotates it at **10 MB**, keeping **5** rotated files (`node.log.1` …
`node.log.5`): at most about 60 MB in all. A node.log already past
10 MB when the node starts is rotated at once, so the mini's 339 MB
file becomes `node.log.1` on the first start of a release carrying
this, and is deleted after five more rotations.

**What moved out.** Channel mode's `session: …` lines, every workspace's
git commands and output, and the agents' stderr go to
`~/.mesh/sessions.log` (20 MB, 2 kept). node.log keeps the node's own
reasoning — ticks, claims, reloads, heartbeats, holds — and every line
that summarises a session (start, end, usage, cost, what its end
killed). The error that ends a start is now logged there too
(`exiting: …`); before, it reached node.log only because the service
appended stderr to it.

**The service.** The systemd unit's `StandardOutput` and
`StandardError` are `journal`; the LaunchAgent's are
`~/.mesh/node.crash.log`. **A unit installed before this change keeps
`StandardOutput=append:~/.mesh/node.log` until `mesh node service
install` is run again** — the mini's included; this build did not touch
it. Until then the node rotates node.log regardless, and a panic's stack
lands in whichever `node.log.N` systemd's descriptor has followed.

**Tests.** `TestRotatingFileRotatesAtTheLimit` (writes past a 1000-byte
cap: `node.log.1` exists at 1000 bytes, node.log at 100; far past it,
node.log stays under the cap and exactly 3 rotated files are kept),
`TestRotatingFileRotatesAnOversizedLogOnOpen`,
`TestRotatingFileOpenFailsWhenAnOversizedLogCannotRotate` (a 5000-byte
node.log over a 1000-byte cap with `node.log.1` a directory: the open
returns `log <path>: rotate on open (5000 bytes, cap 1000): …` and leaves
the file whole — the first attempt's evaluator found it panicked there,
dereferencing the file `rotate` had already closed, the path the mini's
oversized log takes on its first start),
`TestRotatingFileCutsAWriteLargerThanTheCap` (a 5000-byte write to a
1000-byte cap leaves node.log at exactly 1000 bytes ending in the cut
line, the earlier line in `node.log.1`),
`TestRotatingFileRefusesAWriteItCannotRotateFor` (with `node.log.1` a
directory the rename fails: five writes are refused with the rotate
error, node.log stays at the cap, one line is said; once it can rotate
the next write lands in a fresh file and the recovery is said),
`TestNodeLogsKeepSessionsApart` and
`TestChannelTextLandsApartFromNodeLines` (a node line and a `session:`
line in different files, whole lines asserted),
`TestServiceOutputIsNotNodeLog` (the rendered unit's output lines are
exactly `StandardOutput=journal` and `StandardError=journal`; the plist's
paths are node.crash.log).

**Measured.** No hour-long run was possible in the build session: this
dev box is not a registered node, and registering it would have put it
on the platform claiming real work. The run that was possible was the
branch's binary started as a service would (`mesh node start --mode
poll`, stdin and stderr not a terminal) with a fresh `HOME`: it ran
**8 ms**, to the registration check, and left a **271-byte** node.log of
three lines — the version and pid, `no update pending`, and `exiting:
this machine is not registered …` — and an empty sessions.log. Started
the same way over a 30 MB node.log, it rotated that to `node.log.1`
(31457280 bytes) and wrote a 271-byte node.log. The size node.log
reaches after an hour of work on the mini is owed once the mini runs a
release carrying this and its unit is reinstalled.

**On the mini.** The mini has not yet run a release carrying this
item: it is on v0.2.191 (`mesh_health_nodes`, confirmed again this
session at 23:39:53Z), 21 commits behind main; the first release with
the rotation is v0.2.205 (`## The mini before and after`, above). This
session has no shell on the mini (`ssh mini` refuses this session's
key, tried again while writing this section) and, per this item's
instruction, did not restart or reinstall its unit to get a fresh
reading — but item 7's own build session did have one, at 2026-09-29
22:25Z, and read rather than changed it: `mesh v0.2.191 (commit
8eba662)`, which does not carry this; `io.projectmesh.mesh-node.service`
still has `StandardOutput=append:/home/chris/.mesh/node.log` and the
same for `StandardError`; `~/.mesh/node.log` is **373,995,651 bytes,
6,548,269 lines** (339 MB when the goal was set, so about 35 MB in the
day since), 1,239 of them channel `session:` text, and no
`sessions.log` exists — the unit still has the append redirect, so
node.log is still unbounded there regardless of the rotation code
being on main. The operator owes two steps, in order, once ready:
upgrade the mini to v0.2.208 or later (the release path, or by hand
with `mesh node upgrade --binary`) — v0.2.205 alone would carry the
rotation, but v0.2.208 also carries items 8 and 9 — then run `mesh node
service install` again so the unit's `StandardOutput`/`StandardError`
drop the `append:~/.mesh/node.log` redirect for `journal` — rotation
runs regardless of the unit, but the stale redirect keeps the shell
appending the same growing file underneath it until the unit is
reinstalled. node.log's and sessions.log's sizes after an hour of real
work on the mini are owed once both steps are done.

## What closed

**The four possibilities in the Statement, and which held:**

- (a) reload not called on this mode, or (c) `Reloader` returning early
  before capacity was set — both ruled out: the 06:15:46 and 06:17:09
  lines (`## Root cause`) show the reload ran on schedule and capacity
  was applied and logged twice in the same window.
- (b) `LoadConfig` reading a different file — ruled out: the service's
  `HOME` and the running process's own environment both resolve to
  `~/.mesh/node.yaml`, the file the operator edited (`## Root cause`,
  "The service reads the file the operator edited").
- **(d) held: the loop applied the new capacity and logged it, but the
  platform was never told.** `Loop.heartbeatScaling` sent `scaling` for
  a fixed node only on two or more accounts; the mini's one account got
  none, so `NodeIndex.capacity` stayed at its last registration (6)
  until the 14:13–14:25Z restart re-registered it at 8. Fixed by
  heartbeating `scaling` for a fixed node on any number of accounts
  (ADR-018).

**The exit's cause:** an unrecovered nil-pointer panic in
`Evaluator.Evaluate` (`evaluate.go:262`, v0.2.188) — a Codex preflight
failing on a 429 turned the evaluation to Claude, `Credentials.Open`'s
error return overwrote the deferred-over `acct` with `nil`, and
`(*SessionAccount)(nil).Cleanup()` reached through it on the way out.
Nothing recovered it (the goroutine was `evaluateHeld`'s), so the whole
node died: exit status 2, the Go runtime's unrecovered-panic code, not
an argument. Fixed by giving `Open`'s result its own variable so the
error returns as an ordinary evaluation error instead, and every
goroutine the node starts now runs under `crash.Guard`, so any future
panic anywhere is one `mesh node: fatal: … (version <v>)` line and exit
70, never a silent 2.

**node.log's bound:** 10 MB per file, 5 rotated files kept (about 60 MB
in all), session transcripts and other sessions' own output moved to
`~/.mesh/sessions.log` (20 MB, 2 kept) so node.log stays the node's own
reasoning.

**The items, with their pull requests:**

| Item | What it landed | PR |
|---|---|---|
| 3 | The 04:00:22Z exit explained and fixed; a panic anywhere names itself and exits 70 | #493 |
| 6 | The capacity-reload test that fails today, and the fixed-node `scaling` heartbeat that fixes it | #494 |
| 7 | node.log bounded and rotated; session transcripts to their own file | #495, carried onto main as #503 |
| 8 | Every reloadable field — limits, accounts, budgets, context, claim, orgs — proved live from disk and logged with its effective old and new value | #504 |
| 9 | A reload that cannot be applied logs one `reload config:` line per tick and keeps the last good values | #502 |

**The ADR note.** PR #495's two commits (item 7) were cherry-picked onto
main after #493 without a conflict — branch 4 already sat on #493's
merge, so `crash.Guard`'s wiring in `nodecmd.go` and the exit section
above are kept. The ADR is **ADR-019**, not 018: PR #494 (item 6's
carry) claims `adr-018-a-fixed-node-heartbeats-its-capacity`.
