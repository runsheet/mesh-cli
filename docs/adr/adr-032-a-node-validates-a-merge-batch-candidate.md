---
title: "ADR-032: A node validates a merge batch candidate"
summary: Every tick, beside the evaluations, the node reads mainValidations and claims each merge batch candidate (then each merged item's main validation request) of an org it serves while it has a free slot, runs the repository's .runsheet/validate.yml in the sandbox on exactly the candidate's sha, records the attestation with mergeBatchId, and finishes the batch — green or red, since the attestation is the verdict — failing it with the reason only on an environment failure, an error or a run past the claim's window; each validation counts against capacity and logs `batch <repo>#<n> attempt <a>: …`.
status: accepted
---

# ADR-032: A node validates a merge batch candidate

2026-10-14, goal `a-node-validates-a-merge-batch-candidate` (items 1–5).
The node's half of runsheet ADR 048 (*the integrator merges a batch on one
run*, decision 3) and ADR 054 (*the node's attestation is the verdict*,
amendment 2026-10-13). Builds on [ADR-013](adr-013-validation-runs-from-a-declared-config.md)
and [ADR-014](adr-014-a-build-validates-before-it-pushes.md) (validate.yml,
the attestation), [ADR-030](adr-030-an-environment-failure-names-its-kind.md)
(an environment failure names its kind), [ADR-028](adr-028-one-clone-per-repo-one-worktree-per-item.md)
(one clone per repo) and [ADR-025](adr-025-a-node-says-which-sessions-it-takes.md)
(`takes:` caps).

## Context

The integrator merges a queue of attested pull requests onto a candidate
branch and asks a node to prove the candidate — main plus the batch —
before main fast-forwards to it (ADR 048 decision 3). The platform serves
the asks on `mainValidations` and the commands to claim, finish and fail
them, and `recordValidationAttestation` takes a `mergeBatchId`. No node
ever claimed one: mesh-cli's schema did not carry the fields and its loop
had no path for them. `mergeBatchIndexs(where repo_id = ce9057d5-…)` on
2026-10-14 shows latchkey's batches waiting on a node with no claim, and
the dl360 node logs never mention a batch. Merged items' main validation
requests (ADR 054 decision 3, the main guard asking for a run on a merge
commit) waited the same way.

## Decision

**What the node polls, and when.** Every tick, after the sessions in use
are counted and before `claimReady`, `Loop.validateBatches`
(`internal/node/mainvalidation_loop.go`) reads `mainValidations(nodeId)`
for each of the node's ids (`gateway.MainValidations`,
`internal/gateway/mainvalidation.go`): `batches[]` — `{mergeBatchId,
number, candidateBranch, sha, attempt, repoFullName, repoId, orgId,
defaultBranch, requestedAt}` — and `requests[]` for merged items. It runs
only when node.yaml's `takes:` takes `evaluate` (a validation is a check,
as an evaluation is, and sits under its cap), the node has a free slot,
the host is not hot (ADR-023/ADR-031) and the node is not draining. An
entry of an org the node does not serve is skipped. Every batch is
offered before any request: a batch lands work already built and
attested. A platform without the fields (`TakesMainValidation`:
`Query.mainValidations` and the six mutations, introspected and kept
`ScalingProbeEvery`) is asked nothing and said once per run, `the
platform serves no main validation; validating no merge batch`.

**Claim once, converge.** `ClaimMergeBatchValidation(batch, nodeId,
harness)` (or `ClaimWorkItemMainValidation` for a request) is idempotent
on the same node and refused for another's; a refusal is the ordinary
answer and is not logged. While the run goes on the node asks for the
claim again every `BatchCheckEvery` (default a minute): a refusal is the
claim lost, the run stopped and nothing sent. A run past
`MainValidationClaimAfter` (10 minutes, the platform's claim window) is
stopped and failed with why.

**Capacity.** A running validation is a session: counted in `Running()`,
in the drain's `sessions()` and in `runningOf(evaluate)` for the `takes:`
cap, its slot freed when it ends.

**The run.** A batch: `Runner.validateCandidate` checks the candidate
branch out as a detached worktree of the repo's shared clone at
`<work root>/merge-batches/<mergeBatchId>-<attempt>` (`CandidateDir`,
`Git.AddCandidateWorktree`), refusing a fetched head other than the
batch's `sha` (`*CandidateHeadError`) with nothing run or attested; runs
the suites the diff from `origin/<defaultBranch>` selects, plus every
`always` one, in a sandbox pod of kind `validate` with the checkout's
services, as a build's validation does (`validator.start`/`run`); and
uploads with `RecordBatchCandidateAttestation` —
`recordValidationAttestation` carrying `mergeBatchId` beside the same body
an item's attestation sends. A request: `validateMainRequest` checks the
merge commit out at `<work root>/main-validations/<workItemId>-<attempt>`
(`Git.AddCommitWorktree`; the default branch need only hold the sha, else
`*CommitNotOnBranchError`), selects the suites by the merge's diff from
its first parent, and attests it as the item's, with no `mergeBatchId`.
The worktree goes on every path. The platform mints no repo-keyed token
yet, so both run on the operator's credentials.

**Outcomes.** Green and red alike are **finished** on the attested sha
(`FinishMergeBatchValidation`, `FinishWorkItemMainValidation`): the
attestation is the verdict (ADR 054's amendment), and the platform reads
its colour. A step with an environment kind (ADR-030), a classified fetch
failure, a required service that does not come up or any other error is
the node's failure, not a verdict: nothing is attested and the claim is
**failed** with the reason (`FailMergeBatchValidation`,
`FailWorkItemMainValidation`, `validate.Reason`'s `<kind>: …`), which
releases it for another node.

**The WorkItem twins.** A request takes the same path as a batch
(`mainJob`: `startMainJob`, `runMainJob`, `watchMainClaim`) through
`ClaimWorkItemMainValidation`, `FinishWorkItemMainValidation(sha)` and
`FailWorkItemMainValidation(reason)`.

**The log lines.** Each step is one node.log line:
`batch <repo>#<number> attempt <n>: claimed candidate <branch>@<sha7>`,
`… attested green|red (<suites>)`, `… finished`, `… failed: <reason>`,
`… stopped: claim lost …` or `… stopped: ran past <window>, the claim's
window; stopped`; for a request the prefix is `main <repo>@<sha7> item
<seq>: ` and the claim line `claimed`.

## Consequences

- A merge batch completes once a release reaches the nodes: latchkey's
  candidate is claimed, validated and finished, and main fast-forwards to
  it on green.
- Batch validations take node capacity and the `evaluate` cap: a node
  full of builds validates no batch until a slot frees, and `takes:`
  without `evaluate` validates none.
- The operator's `gh`/`git` credentials fetch the candidate until the
  platform mints a token keyed by repo; the attestation still names the
  node's runner image.
- The contracts under `specs/contracts/` (`mainValidations.json`, the
  three batch commands, `recordValidationAttestation-batchCandidate.json`,
  the three WorkItem twins) were written from the live schema, not copied
  from runsheet; the `requests[]` entry's shape and the receipt's `branch`
  are to be checked against runsheet's `gateway/mainvalidation.go`.

## Proof

- Item 1, the gateway: `TestMainValidationsDecodesTheContract`,
  `TestMainValidationCommandsSendTheContracts`,
  `TestRecordBatchCandidateAttestationSendsTheContract`,
  `TestTakesMainValidationFalseWithoutTheFields`,
  `TestSchemaHasMainValidation` and the fake's
  `TestFakeMergeBatchClaimConvergesAndFinishes` (`internal/gateway/`).
- Item 2, the candidate's run: `internal/node/mainvalidation_test.go` —
  `TestCandidateGreenIsAttestedWithMergeBatchID`,
  `TestCandidateRedSuiteIsARedAttestation`,
  `TestCandidateHeadOtherThanBatchSHAIsRefused`,
  `TestCandidateEnvironmentFailureIsReturnedWithItsKind`.
- Item 3, the loop: `internal/node/mainvalidation_loop_test.go` —
  `TestBatchClaimedValidatedAndFinishedGreen`,
  `TestBatchRedCandidateIsFinishedRed`,
  `TestBatchEnvironmentFailureFailsTheValidation`,
  `TestBatchClaimedByAnotherNodeIsNotRun`, `TestBatchCountsAgainstCapacity`,
  `TestBatchDrainingClaimsNone`, `TestBatchPlatformWithoutMainValidation`,
  `TestBatchClaimLostStopsTheRun`, `TestBatchOverrunIsStoppedAndFailed`.
- Item 4, the WorkItem twins: `internal/node/mainvalidation_request_test.go`
  — `TestMainRequestClaimedAttestedAndFinished`,
  `TestMainRequestEnvironmentFailureFailsIt`,
  `TestMainBatchServedBeforeRequest`,
  `TestMainRequestCountsAndStopsOnClaimLoss`.
- Item 5, end to end: `internal/node/mergebatch_e2e_test.go`'s
  `TestNodeValidatesAMergeBatchCandidate` runs the node's `Loop.Run`
  against the gatewaytest fake with `sandbox.Fake`: a seeded batch is
  claimed, its suites run in the pod on exactly the candidate's sha, its
  attestation recorded with `mergeBatchId` and the batch finished, with
  the `claimed candidate`, `attested green` and `finished` lines; a
  failing suite is attested red and never green; a merged item's main
  validation request is claimed, attested as the item's and finished.
