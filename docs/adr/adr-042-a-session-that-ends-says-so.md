---
summary: A session that ends reaches the platform — a build session that exits at once is failed (or, for the infrastructure, released) with its reason inside a minute, an evaluation heartbeats its item from claim to verdict so a long suite is never stalled, and every finish, fail, release or verdict the gateway refuses is logged with the gateway's answer through one helper, handInRefused, which a guard test keeps every hand-in on.
status: accepted
---

# ADR-042: A session that ends says so

2026-10-26, goal `a-session-that-ends-says-so-and-a-long-one-stays-alive`.
Amends ADR-024 (a workspace failure releases the claim) and ADR-029 (a
session the node gives up on is stopped) on what the node sends when a
session ends; the platform's stall sweep is runsheet ADR 018's
amendment 2026-10-18.

## Context

On dl360 (node v0.2.529) a build session ended forty seconds after its
claim — `tools: context7 served, never called; runsheet served, never
called: failed`, seven turns, a `mesh_finish` call the platform never
saw — and nothing reached the platform until the item was stalled and
re-claimed fifty minutes later. The node logged about twenty such
endings an hour. Evaluations of items whose acceptance runs long suites
(three full e2e shards) were stalled at 84 and 97 minutes: the evaluator
sent no heartbeat while its session ran, and the platform stalls an
evaluating item whose heartbeat is 15 minutes old. And a finish or
release the gateway refused was often dropped (`_ = gw.FailWorkItem(…)`)
or logged without the gateway's answer, so a refused hand-in looked, in
node.log, like silence.

## Decision

1. **A session that exits at once is reported inside a minute.** A
   build that ends unsuccessful, "done" with no `mesh_finish` and
   nothing committed, on a harness error, or with its `runsheet` server
   failed, is failed (`failWorkItem`) with the session's text or the
   tools note; one whose pod the runtime could not start is released
   (`releaseWorkItem`, ADR-030's runtime release). The fail and the
   release are sent by `sendReport` — retried on a transient answer
   within `ReportRetryFor` (60 s) — and a fail refused, or never
   answered inside the minute, releases the claim naming the reason
   (`Runner.releaseNaming`): the item is never left claimed for the
   stall sweep. `TestSessionThatExitsAtOnceIsReportedE2E` (`-tags e2e`,
   `internal/node/session_exits_e2e_test.go`) runs the five endings on
   the fake gateway and sees each reach it within 60 s.
2. **An evaluation heartbeats from claim to verdict.** `Evaluate` and
   `AdoptEvaluation` run a ticker (`Evaluator.HeartbeatEvery`, else
   node.yaml's `evaluate.heartbeat_every`, default 5 minutes,
   `DefaultEvaluationHeartbeatEvery`, refused at or past the platform's
   15-minute window) sending `ReportWorkItemProgress` at 0 cents naming
   the phase (checkout, session, second opinion), stopped
   (`stopHeartbeat`) before the verdict, no-verdict or release and on
   every other exit; a beat refused as not held stops the evaluation as
   a lost claim (`ErrClaimLost`). `TestEvaluationOutlivesTheStallWindowE2E`
   (`-tags e2e`, `evaluate_heartbeat_e2e_test.go`) runs a session past
   the fake's `StallWindow` and sees no stall, one claim and one verdict;
   its `…Control` twin with no beats sees the stall.
3. **Every refused hand-in is logged with the gateway's answer.** A
   hand-in — `FinishWorkItem`, `FinishWorkItemAlreadyDone`,
   `FinishWorkItemChangeset`, `FailWorkItem`, `ReleaseWorkItem`,
   `ReleasePlan`, `EvaluateWorkItem`, `EvaluateWorkItemNoVerdict` — hands
   its error to `handInRefused(logf, redact, label, call, err)`
   (`internal/node/handin.go`), which logs `item <label>: <call>
   refused: <the gateway's answer>`, redacted, and returns err
   unchanged, so the caller's own handling (`evaluationNotYours`, the
   fail after a refused release, `finishRefused`) goes on as before. It
   is called on what is left once the usage report's
   resend-without-a-field answers are absorbed (`acct.Carried`,
   `validationUsage`, the changeset finish's `c.carried`). A refused
   `ReleasePlan` is logged in its refused branch only — an unsupported
   release is not a refusal — beside the existing `plan release refused`
   lines, labelled by the goal's slug; the evaluator's `EvaluateWorkItem`
   (`failDiff`, `afterEvaluate`) and `EvaluateWorkItemNoVerdict` (after
   its `ErrNoVerdictOutcome` check) the same. A fail or release sent by
   `sendReport` is logged through it only for a refusal that is not
   transient; a transient one past the window keeps its own line.
   `TestHandInRefusalIsLogged` (the runner),
   `TestHandInRefusalIsLoggedByThePlanner` and
   `TestHandInRefusalIsLoggedByTheEvaluator` drive each path to a refusal
   on the fake.
4. **A guard keeps every hand-in on the helper.**
   `TestEveryHandInErrorGoesThroughTheHelper`
   (`internal/node/handin_guard_test.go`) parses every non-test file of
   `internal/node` with `go/parser` and fails, naming `file:line`, on a
   hand-in whose error does not reach `handInRefused`: the call is
   inside its argument (directly or wrapped by `acct.Carried`,
   `validationUsage` or `c.carried`), or its error is assigned —
   `err := gw.X(…)`, an `if`'s or `switch`'s init, or `sendReport` over a
   func returning the call — to a variable a later `handInRefused` in
   the same function takes. A discarded call (`_ = gw.FailWorkItem(…)`),
   a bare statement, or one assigned and never handed on is reported;
   its `bites` sub-case runs the checker on in-memory sources to prove
   it. The interfaces declaring the methods are no calls, and a method
   of the package named for one (a forwarding wrapper) is skipped.

## Consequences

- A new hand-in, or a new call site of one, logs its refusal or fails
  the guard; a new hand-in method is added to `handInMethods` and a new
  usage wrapper to `handInWrappers` in the guard.
- gatewaytest's `Fake` records no `Calls` entry for a mutation it
  answers by `Refuse`, so a test of a refused hand-in reads the log
  line, never `CallsNamed`.
- The mesh-specs TRD §3 paragraph on a session's end (the fail inside a
  minute, the evaluation's heartbeat, the refusal logged) is owed to a
  follow-up in that repository.
