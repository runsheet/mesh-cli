---
summary: On a v1 node run_suite runs a sharded suite's shards (and its groups) at once, the first in the lease's pod and each other in a pod of its own beside it with its own copy of the lease's services, on the lease's HOME, network and egress (amended 2026-11-02: a lane sees what its lease's pod sees), on the lease's resources held of the node's scheduler; a run longer than the call's wait (10 minutes) answers {run_id, status: running}, which the session polls with run_suite {run_id}, never an MCP timeout; fail_fast stops the other shards at the first failure; a validate lease is held to its validate.yml budget from the lease's start, past which the run is stopped and answered red, out_of_time, with its signed record; the record stays one per run, its output blob naming each shard.
status: accepted
---

# ADR-049: A v1 suite's shards run at once, inside its budget

2026-11-02. Mesh v1. mesh-specs gap 36. Applies to
`internal/nodetools` (run_suite), `internal/nodev1` (lanes.go,
podsession.go, tools.go), `internal/validate` and `internal/sandbox`.
Builds on ADR-013's amendment of 2026-10-28 (groups and shards run at
once), which the legacy node had and the v1 node did not.

## Context

runsheet's `.runsheet/validate.yml` gives its integration suite
`shards: 4`, each meant to run in a pod of its own with its own
Postgres, all at once, inside an 80-minute `budget`. On a v1 validate
lease the session calls run_suite, which ran a sharded suite's shards
one after another in one exec container of the lease's pod (lease
27dd302c on dl360-v1, 2026-11-01: shard 0 from 17:21 to about 18:21,
shard 1 from 18:28). Each shard's own timeout is 60 minutes, so a red
change could hold a validate lease for four hours. The session's MCP
call gives up at 30 minutes, under one shard's hour, and validations
were handed back cannot_run ("MCP tool timeouts at 30m for both progress
and run_suite (integration)", lease ad0aeda8). Nothing held a lease to
the repository's budget.

## Decision

**A sharded suite's shards run at once.** run_suite's runner on a
sandboxed lease is a `validate.LaneRunner`: `validate.Run` runs each
group, and each shard of a step declaring `shards:`, in a lane of its
own. The first lane runs in the lease's own pod. Every other lane is a
pod of its own beside it (`<lease pod>-lane-<n>`):

- **its own copy of the lease's services**: the checkout's
  `.mesh/services.yml` and the brief's pods, started and health-checked
  as the lease's are, so each shard has its own Postgres on
  `127.0.0.1:5432`. A required service that is not ready fails the lane,
  never a shard run without its database.
- **the lease's resources**, held of the node's scheduler (owner
  `<lease>/lane-<n>`, the brief's CPUs and memory, the build cache
  shared, renewed every 30 s) as any lease's are: on a busy node a lane
  waits for room rather than over-commit it. It takes no job slot.
- **no network** (`sandbox.Isolated`). The lease's relay is a sidecar of
  the lease's own pod, or a bridge in its agent's container, which a
  lane cannot reach, so a lane reaches its services and the node's
  package stores and nothing else. A suite that needs the network to
  fetch its dependencies fetches them in the lease's pod first (any
  earlier suite there does, as runsheet's lint and unit do).
- **the lease's labels**, so the supervisor's reconcile removes a lane a
  dead node left behind. A lane's pod is removed, and its scheduler hold
  given back, when its steps have run.

A command stopped under a pod (fail fast, the budget, the lease's end)
has its container removed at once (`sandbox.Session.Exec`), not when
its pod goes.

**A run outlives its call.** run_suite starts the run in the lease's
context and waits at most `Runtime.SuiteWait` (
`nodetools.DefaultSuiteWait`, 10 minutes: under the 30 minutes a
session's MCP call gives up after). A run that has not ended answers its
handle, carrying every test streamed so far (`validate.WithOnTest`), the
same bounded `tests` list (and `tests_truncated` when it was cut) a
finished answer carries:

```json
{"run_id": "run-1", "status": "running", "suites": ["integration"],
 "started_at": "…", "elapsed_s": 600, "budget_left_s": 3900,
 "tests": [{"name": "TestFoo", "package": "./e2e", "outcome": "fail",
            "duration_ms": 120, "output": "…"}],
 "next": "the run goes on: call run_suite {\"run_id\": \"run-1\"} to wait for it again"}
```

`run_suite {run_id, wait_seconds?}` waits again (at most the node's
wait) and answers the same handle, its `tests` list grown by whatever
streamed meanwhile, or, once the run has ended, its result, now with
`run_id` and `status: finished`, its own `tests` list `Normalise`'s —
never the streamed one, which is live and unauthoritative; a finished
run's id answers its result again. A call its caller gave up on leaves
the run going. The run ends with the lease, never with a call.

**fail_fast.** `run_suite {…, fail_fast: true}` stops the run at its
first failing suite or shard: the lanes still running are cancelled, and
their shards named not run (`validate.Options.FailFast`, asked through
the runner's ctx by `validate.WithFailFast`). Without it every shard
runs to its end, as before.

**A validation is held to its budget.** On a lease whose kind is
`validate` (bare or under its product), run_suite reads the checkout's
`validate.yml` `budget:` when the lease's tools are made (before the
session can write the file) and holds the lease to it from the lease's
start: a run still going at start + budget is stopped there and answered
red — `exit: 1`, `out_of_time: true`, `budget`, a summary beginning
"ran out of time: the lease ran past the repository's validation budget
of 80m" — and a run asked for after it runs nothing and is answered the
same. Either is a signed run_suite record of the red, so the session
finishes red with the reason and the product's finish guard has its
evidence. A run that passed whole is green however close it came. Other
kinds (build, steward) are held to no budget. `validate.Config.Budget`
parses `budget:` (a positive Go duration; runsheet's validatespec
bounds it 1m to 3h).

**The record is one per run.** A run_suite run is one signed record, as
before, whatever its shards: its predicate's shape is unchanged, so
Mesh's finish guard, `waived_by` and runsheet's attestation read it as
they did. Its output blob (signed by digest) and the call's answer name
each shard of a sharded step, additively:

```json
{"name": "integration", "exit": 1, "duration_ms": 3410000, "tests_run": 812,
 "shards": [{"index": 0, "total": 4, "exit": 0, "duration_ms": 3300000, "tests_run": 205, …},
            {"index": 1, "total": 4, "exit": 1, …},
            {"index": 2, "total": 4, "exit": -1, "not_run": true}, …]}
```

A shard is never a record of its own: a shard's record would read as
the suite's full run to a product that does not know shards, which gap
27's `selection` exists to prevent.

## Consequences

- runsheet's integration suite on a v1 head validation takes about one
  shard's time (its four shards at once on 4 CPUs and 8 GB each, 16 CPUs
  and 32 GB of dl360-v1's 32 and 64), not four, and never meets the
  MCP call's timeout.
- A validation past its budget ends red with the reason instead of
  holding its lease for hours; a slow red change costs at most its
  budget.
- ~~A lane has no network: a sharded suite that downloads at run time
  fails in its lanes until the lease's pod has the dependencies.~~
  Wrong, and it broke every runsheet validation: see the amendment
  below.
- The non-pod runners (a node with `--sandbox` off but a sandbox
  runtime, and the host runner) still run shards one after another; they
  get the handle and the budget all the same.
- Mesh's API, the kind's brief and the evidence contract are unchanged:
  run_suite's schema is the node's own (`run_id`, `wait_seconds`,
  `fail_fast` and, on both the running handle and the finished answer,
  `tests`/`tests_truncated` added), so no projectmesh-io/api amendment.

## Amendment 2026-11-02: a lane sees what its lease's pod sees

**What broke.** Deployed to dl360-v1 at 14:22 AEDT, every runsheet
validation's integration shards 1–3 failed in 1–3 s with 0 tests, on
each of three attempts, while shard 0 ran its 879; 23 validations were
handed back cannot_run, and the node was rolled back at about 15:45.
A lane was opened as a fresh sandbox session (`sandbox.Open`): an
**empty HOME** of its own and **no network**. On a v1 node Go's module
cache (`~/go/pkg/mod`), build cache (`~/.cache/go-build`) and
`~/go/bin` are in the session's HOME — no node store holds them — and
the lease's pod fills them through its relay (lease 30bcf23f's HOME:
985 MB of modules, 2.3 GB of build cache). A lane saw none of it and
could fetch nothing, so `go test ./e2e` failed at setup:
`dial tcp: lookup proxy.golang.org: ... network is unreachable`,
`FAIL e2e [setup failed]`, exit 1 (reproduced with runsheet's
integration command in a `--network none` container on an empty HOME).
The node's log said only that each lane opened. The Consequences'
"fetches them in the lease's pod first" never held: what the lease's
pod fetched was in a HOME the lanes did not mount.

**Decision.** A lane sees exactly what a step in the lease's pod sees,
but for its pod, its own services and its scheduler hold:

- **the lease's HOME** (`sandbox.Sandbox.OpenBeside`), mounted
  read-write at its own path and named `HOME`: Go's module and build
  caches, `~/go/bin`, tools a step installed, the relay's CA and every
  file the session or a step wrote there. Go's caches are safe for
  several processes at once. A lane's end removes its pod, never the
  HOME, which is the lease's. A lane writes no services listing of its
  own: its services answer where the lease's do, on its own loopback.
- **the same checkout, image, `MESH_ITEM`, kind, hooks, environment and
  PATH**, and the lease's `proxyEnv` (with the relay CA's trust
  variables) and `registryEnv`.
- **the lease's network and egress, never a raw network.** A lane's pod
  is on the lease's pod's network (`leaseNetwork`): none but its
  loopback with a relay sidecar, relay-only with a kind's egress, the
  sandbox's otherwise. Where the relay is the only exit, each lane step
  runs behind the git bridge (`node <HOME>/mesh-lane-bridge.js <socket>
  <port> -- sh -c <command>`), listening on the lane's loopback at the
  port the lease's `HTTPS_PROXY` names and piping to a socket mounted
  into the lane: with a relay sidecar, the node's **lane tunnel**
  (`relaySidecar.openLaneTunnel`, `<lease socket dir>/l/s`), which
  answers CONNECT alone on the lease's egress tunnel (its policy, its
  interception, its audit, as the session's CONNECT through the sidecar
  is) and refuses every other route of the node's side: no renewal, no
  credentials, no node tools, no model; without one, the lease's
  sessionproxy socket, as the lease's pod's bridge reaches it. The lane
  tunnel closes with the lease. The sidecar's plain-HTTP routes
  (`/git/`, the registry) are not served to lanes; a step needing them
  runs in a group of the lease's pod.
- **a failed step is in the node's log** (`podSteps.logFailedStep`),
  in a lane or the lease's own pod: where it ran, its shard, its exit
  (or why it did not run), how long it took and the last eight lines of
  its stderr, redacted of tokens. Each lane's open line names its HOME
  and its egress.

`TestEveryShardSeesTheLeasesGoModuleCache` fails on the change before
this one exactly as dl360 did (shard 1: exit 1, 0 tests);
`TestALaneReachesTheLeasesRelay`, `TestTheLanesTunnelServesTheLeasesEgressAlone`
and `TestALanesFailedStepIsInTheNodesLog` hold the rest.

## Amendment 2026-11-07: stop_suite ends one lane without leaving a pod

A shard stuck past the point it is worth waiting on — a hung process, a
lane plainly not going to pass — held its lease to the whole budget or
the session's own 30-minute timeout, for want of a way to end it alone:
stopping the run_suite call stops none of the lanes it is polling.

**Decision.** `stop_suite {run_id, step, index?, reason}` ends one lane
of a run still going — a sharded step's one shard (`index`), or a step
with no shards (no `index`) — while the run's other lanes go on:

```json
{"run_id": "run-1", "step": "integration", "index": 1,
 "reason": "stuck: no output for 12 minutes"}
```

`run_suite`, opening a lane (`validate.Options.OnLane`, asked through
the runner's ctx by `validate.WithOnLane` as `FailFast` and `OnTest`
are), keeps each lane's own cancel under its name — `"<step>"` with no
shards, `"<step>-<index>"` with one, the same name `MESH_SHARD_INDEX`
carries — so stop_suite reaches a lane only through the run that opened
it, naming an unknown run or lane refused. The cancel runs the lane's
command's ctx down, so its pod's container (and a lane's own pod,
beside the lease's) is torn down at once as fail_fast's lanes are
(`sandbox.Session.Exec`); the lease's own pod and the run's other lanes
are untouched.

**The record gains a stop, nothing else.** The stopped lane's
`StepResult` (and, merged, its sharded step's) carries `stopped: true`
and `stop_reason`, beside its ordinarily non-zero exit, in both the
running handle's and the finished answer's `shards`/`steps` and the
signed output blob — the predicate's shape otherwise unchanged, so a
product reading it as before reads a failed shard with a reason, not a
new field it must learn. A stopped shard's exit is never classified as
an environment failure (`EnvironmentKindOf`) and never folds into
run_suite's own runner error: it is the stop asked for, not a tool or
host failure. A run with one stopped lane is never green: run_suite's
own `exit` is non-zero once any shard is stopped, same as any other
failing shard.

Stopping a lane is a deliberate end, not a retry: the run is not
restarted and the stopped shard is not re-run. A reason naming no open
lane (it has not opened yet, or has already ended) is refused, so a
session stopping a lane always knows whether it took.
