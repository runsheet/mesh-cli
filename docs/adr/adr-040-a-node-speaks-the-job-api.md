---
summary: Every claim, claim-time read and finish of runsheet's kinds goes through the job API (runsheet ADR 061) — claimJob, jobAccess, finishJob — routed in the gateway client (gateway.jobRoutes, IssueItemRepoToken/Session/Plan/Research/batch/main tokens via jobAccess), so the node claims a job of a kind, is handed every token the kind's access names in one exchange, and hands back the kind's finish; a validation's attestation and the end of its claim are one finishJob; a main validation fetches on a read token from the exchange instead of the operator's login.
status: accepted
---

# ADR-040: A node speaks the job API

2026-10-22, Chris: "keep going with the job spec stuff now, get it all
100% done before we start the new server node". Follows runsheet ADR 061
(Mesh is a job system, and the connected products define the jobs),
whose platform half — `claimJob`, `jobAccess`, `finishJob` and the kind
catalogue — is live on runsheet; this is goal `a-node-runs-any-job`'s
protocol half. Builds on ADR-039 (a session works in its own job
directory).

## Decision

### Claims and finishes are the job system's

`gateway.Client.MutateFields` routes the older claim and finish
mutations of runsheet's kinds to the job API (`internal/gateway/jobroute.go`,
`jobRoutes`):

| older mutation | kind | subject | as |
| --- | --- | --- | --- |
| claimWorkItem | runsheet/build | item | claimJob |
| claimWorkItemEvaluation | runsheet/evaluate | item | claimJob |
| claimWorkItemRebase | runsheet/rebase | item | claimJob |
| claimPlan | runsheet/plan | goal | claimJob |
| claimDirectionResearch | runsheet/research | direction | claimJob |
| claimMergeBatchValidation | runsheet/validate | candidate | claimJob |
| claimWorkItemMainValidation | runsheet/validate | main | claimJob |
| finishWorkItem | runsheet/build | item | finishJob `{changeset}` |
| evaluateWorkItem | runsheet/evaluate | item | finishJob `{verdict}` |
| planGoal | runsheet/plan | goal | finishJob `{plan}` |
| recordDirectionResearch | runsheet/research | direction | finishJob `{research}` |
| finishWorkItemRebase | runsheet/rebase | item | finishJob `{rebasedHead}` |

A finish's value is the older mutation's input, every field of it (the
actor, the usage, a changeset id or a head), less the subject's id: the
platform routes it to the same command, so its refusals are the older
mutation's. A finish names the node that finishes: every session's
context carries it (`gateway.WithNodeID`), and a finish without one is
refused before it is sent. The node's code keeps the older verbs as its
vocabulary; what goes on the wire is the job API.

### One exchange

Every claim-time read is the job's `jobAccess`: a session's accounts and
tools (`gateway.Session`, by its ref's kind and subject), an item's
repository token (`IssueItemRepoToken`, as the kind the session's
context names, `gateway.WithJobKind`: a build's write token, an
evaluation's or rebase's as theirs), a plan's and a research's tokens
per repository, a candidate's batch token, and a main validation's read
token of the merged item's repository (`MainValidationToken`), which it
fetches on instead of the operator's login. An access entry the platform
did not serve is the read's refusal, naming why. The client holds a
whole answer (every entry served) for 30 seconds, never into a
repository token's last five minutes, and drops it on the subject's
claim or finish, so a session's start asks the exchange once. A person's `mesh join`,
which holds no job, still asks its item token itself.

### A validation finishes once

A candidate's or a merged item's validation hands back its attestation
with the end of its claim in one `finishJob` (`FinishValidationJob`),
which records the attestation and finishes; the copy beside the checkout
stays. `FinishMergeBatchValidation` and `FinishWorkItemMainValidation`
are gone from the node.

### What stays its own call

A plan's goal split (`proposeGoalSplit`) and its checkpoints
(`setGoalCheckpoints`), releases and fails (`releaseWorkItem`,
`failWorkItem`, `releasePlan`, `fail…Validation`), progress and
heartbeats: they end no job with its kind's finish. A build's head
attestation is still uploaded before its finish, as the build's own
step.

### Not yet

The prompts are still the node's (`internal/harness/prompt*.go`): a
kind's guidance (`agent:builder`, …) served by the product, and the one
step runner for every kind, are the rest of goal `a-node-runs-any-job`.
The older platform fallbacks of the routed reads (a platform without
`itemTools`, `planGitHubToken`, `issueItemRepoToken`'s batch form) are
gone with them: runsheet serves the job API.

## Tests

The tests of an older platform's fallbacks (no plan, research or batch
token, no `itemCredentials`) are gone with the fallbacks' reach: an
exchange the platform does not serve is a refusal, not an older
platform.

`gatewaytest.Fake` serves `claimJob`, `jobAccess` and `finishJob` by the
platform's routing, through its older handlers, so every node test meets
the same answers and refusals; `TestClaimJobSendsAndReadsTheContract`,
`TestJobAccessReadsTheContract` and `TestThreadMessagesReadsTheContract`
hold the client to runsheet's own fixtures (`specs/contracts`).
