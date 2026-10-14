---
title: "ADR-032: A node validates a merge batch candidate"
summary: Every tick, beside the evaluations, the node reads mainValidations and claims each merge batch candidate (then each merged item's main validation request) of an org it serves while it has a free slot, runs the repository's .runsheet/validate.yml in the sandbox on exactly the candidate's sha, records the attestation with mergeBatchId, and finishes the batch — green or red, since the attestation is the verdict — failing it with the reason only on an environment failure, an error or a run past the claim's window; each validation counts against capacity and logs `batch <repo>#<n> attempt <a>: …`. The candidate is fetched on the batch's platform-minted token (the operator's login only on a platform that mints none; a refused mint fails the validation).
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
The worktree goes on every path. A batch's candidate is fetched on the
batch's own token, minted by the platform (see the amendment below); a
merged item's request still runs on the operator's credentials.

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
- The candidate is fetched on the platform-minted batch token (the
  amendment below), the operator's `gh`/`git` login only on a platform
  that mints none; the attestation still names the node's runner image.
- The contracts under `specs/contracts/` (`mainValidations.json`, the
  three batch commands, `recordValidationAttestation-batchCandidate.json`,
  the three WorkItem twins) were written from the live schema, not copied
  from runsheet; the `requests[]` entry's shape and the receipt's `branch`
  are to be checked against runsheet's `gateway/mainvalidation.go`.

## Amendment 2026-10-14: the candidate is fetched on the batch's minted token

Goal `the-node-fetches-a-batch-candidate-with-its-minted-token` (items
1–3), the node's half of runsheet goal
`a-batch-validation-fetches-with-the-repos-token` (runsheet ADR 009's
amendment 2026-10-14, `specs/contracts/issueItemRepoToken-batch.json`).

**The mint.** `issueItemRepoToken` takes `mergeBatchId` beside
`workItemId` — exactly one of the two — and answers the item form's
`{token, expiresAt, repo, access, permissions}` plus the `mergeBatchId`
asked about: read access on the batch's one repo, given only to the node
holding the candidate's validation claim (the platform knows the node by
the bearer). The client's form is `gateway.IssueMergeBatchRepoToken(ctx,
batch, nodeID)`, beside `IssueItemRepoToken`; an answer naming another
batch is refused, and the gatewaytest fake serves it from
`BatchGitHubTokens` while the batch is claimed (by `TokenNodeID` when
set).

**The fetch.** `Runner.ValidateCandidate` no longer runs on the
operator's login: `NewBatchCred` mints the batch's token for the node id
that claimed the batch and hands it to `validateCandidate` as an
`ItemCred` whose renewal is the same batch form. Every git command of the
run that reaches GitHub — the shared clone's sync, the fetch of
`refs/heads/<candidateBranch>` (`mesh/batch/<owner>/<name>/<n>` as the
integrator names it) and the diff base's fetch of `origin/<defaultBranch>`
(`ChangedPaths`) — carries the token by `OnItemToken` (`withGitToken`:
`GIT_CONFIG_*` on the command, an empty `credential.<github>.helper`
first, which drops the operator's own helper, then the token's), so it is
never written to the shared clone's config. Before each of those fetches
`ItemCred.ForFetch` mints it again when `GitHubTokenNeedsRefresh` says it
is near expiry. A token whose `repo` is not the batch's is refused. node.log
says per batch `merge batch <n> (<repo>): candidate fetched on minted
token (<repo>, <access>)`. A merged item's main validation request is not
changed: `ValidateMainRequest` still runs on the operator's login.

**The fallback.** A platform whose `issueItemRepoToken` takes no
`mergeBatchId` (`gateway.IsUnsupported`), or a gateway with no batch form,
fetches the candidate on the operator's own `gh`/`git` login as before,
said once per run — `the platform issues no merge batch token; fetching
candidates on the operator's own login` (`TokenFallback.NoteBatch`, the
Runner's `BatchFallback`, set in poll and channel mode alike) — and per
batch `candidate fetched on the operator's own login`. The rollout is not
a flag day.

**The refusal.** A mint the platform refuses, or that fails, is the
validation's error, `github token for merge batch <n>: <reason>`: nothing
is fetched — on the operator's login or on any token — run or attested,
and the claim is failed with that reason through `failMainJob`
(`FailMergeBatchValidation`), releasing it for another node. A renewal
refused before any fetch — the candidate's, or the diff base's after the
candidate was fetched on a token that has since come near expiry — is the
same error, `github token for merge batch <n>: refresh github token: …`:
`onFreshItemToken` answers the renewal's error instead of fetching on the
held token, so a candidate is fetched on a fresh token or not at all.

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
- The amendment, the minted token: `internal/gateway/`'s
  `IssueMergeBatchRepoToken` tests (item 1);
  `internal/node/mainvalidation_token_test.go` — the mint, its renewal
  before the candidate's and the base's fetch, the old platform's
  fallback said once, and a refused mint or renewal fetching nothing and
  failing the validation (item 2); and end to end
  `internal/node/mergebatch_e2e_test.go`'s
  `TestNodeFetchesABatchCandidateOnItsMintedToken` (item 3): the
  candidate's repo served over HTTP by `git http-backend` behind a
  server that takes one Basic password alone, the operator's login on
  the machine as git's global credential helper throughout. With the
  mint, the server takes `ghs_batch7` alone and is sent nothing else;
  the node's `Loop.Run` claims the batch, fetches
  `mesh/batch/acme/widgets/7` on the token, runs validate.yml in the
  sandbox on the candidate's sha, records the attestation with
  `mergeBatchId` and finishes it. Without it (`issueItemRepoToken`
  taking no `mergeBatchId`), the server takes the operator's login alone
  and the candidate is validated, attested and finished on it, the
  fallback said. Both run in `go test ./...`.
