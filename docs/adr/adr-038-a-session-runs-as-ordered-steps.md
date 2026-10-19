---
title: "ADR-038: A session runs as ordered steps"
summary: A build is run as an Actions-shaped job of ordered steps (internal/session) — checkout, exchange, pod, run, validate, finish, and release always run — each step's record logged `item <label>: step <n> <name>: <outcome> (<took>)[: <err>]`, a step that hands the item back ending the job (session.End, outcome ended) without failing it; the credential boundary is the job container's edge, so a sandboxed build's ghs_ token rides each git command the node runs and is never written into the checkout (ItemCred env-only, session.Checkout's config check), no GH_TOKEN or gh config dir reaches the pod (sandbox.HeldOutEnv) and the session's bearer stays on the node (the relay endpoint, the session proxy); and the finish records a changeset from the node — the tree, the objects the platform lacks, the changeset, the item finished naming changesetId — when the platform's FinishWorkItemInput takes it (gateway.Client.TakesChangesets), else the node pushes and finishes on the head as before. Amends ADR-001, ADR-007, ADR-014 and ADR-023 Decision 2; follows runsheet adr-061 and ADR 058 step 4.
status: accepted
---

# ADR-038: A session runs as ordered steps

2026-10-19, goal `a-session-runs-as-ordered-steps-like-an-actions-job`
(items 1–7). Follows runsheet `adr-061-mesh-is-a-job-system` (§3, a
session's ordered steps; §4, one exchange) and runsheet ADR 058 step 4
(the changeset). Amends
[ADR-001](adr-001-item-scoped-github-token.md) (the item's token on the
checkout), [ADR-007](adr-007-the-sandbox-on-the-node.md) (the sandbox),
[ADR-014](adr-014-a-build-validates-before-it-pushes.md) (the
validation before the push) and
[ADR-023](adr-023-sessions-finish-by-tool-call.md) Decision 2 (push,
then finish).
It shares its number with
[ADR-038, one proxy](adr-038-a-session-reaches-every-mcp-server-through-one-proxy.md),
landed the same day; the two are read together — that one holds every
MCP credential on the node, this one every GitHub credential.

## Context

`Runner.RunItem` was one function three hundred lines long: resolve,
token, workspace, context, account, bearer, tools, pod, the harness,
then `afterBuild`'s release-or-fail-or-validate-or-push-or-finish, with
a dozen `defer`s letting go of what it held. What a build had done when
it ended was in no one place, and the order of its phases was the order
of its lines.

And the job container held a GitHub credential. The checkout's
credential helper — the item's `ghs_` token, written by `Authorize` into
the worktree's `config.worktree` and rewritten on every refresh — sat in
a directory the pod mounts, and `GH_TOKEN` and `GH_CONFIG_DIR` rode the
pod's env (ADR-001). Anything the agent ran could read either and push
from inside the pod. runsheet adr-061 makes a session a job whose steps
run on the node with one credential exchange, and nothing in the job
container that could reach GitHub; ADR 058 makes the platform, not the
pod, write the branch.

## Decision

### 1. A build is a job of ordered steps

`internal/session` (items 1, 2, 4, 5, 6) holds the shape: `Step{Name,
Always, Run}`, `Job{Steps}`, `Run(ctx, job, state) []Record` — main
steps strictly in order, stopping at the first failure; every `Always`
step afterwards, in order, whatever came before; one `Record{Name,
Started, Ended, Outcome, Err}` per step. `End(err)` is a step's way to
end the job without failing it — the item released, failed by the
node, finished already done — recorded `ended`, the later main steps
`skipped`, the always ones still run.

`Runner.RunItem` resolves the item, makes its spec and asks for its
token as before, then runs these steps:

| # | step | what it does | ends the job when |
|---|------|--------------|-------------------|
| 1 | `checkout` | `PrepareWithRetry`, `Identify`, the context written; sandboxed, `session.Checkout`'s check that no git config of the workspace names the token | a credential refusal (`ReleaseOnCredential`), a workspace that cannot be prepared (`releaseWorkspace`, ADR-024) |
| 2 | `exchange` | the model and budget, the prompt input, the session's account (`Credentials.Open`), its own mesh session's bearer, its tools and proxy | no credential or headroom, no bearer, a required tool missing — each a release |
| 3 | `pod` | `openOrgSessionPod`: the job container and its services | a runtime that cannot start a pod, a required service down |
| 4 | `run` | the prompt (a retry turn, resumed or fresh), the harness, and the session's end read (`buildTail.end`) | a lost or starved claim, a limit kill, a harness error, an unsuccessful end, `mesh_fail`, an already-done finish, the node shutting down |
| 5 | `validate` | `.runsheet/validate.yml`'s suites in the session's pod, a failing step's retry turn (ADR-014) | a validation failure |
| 6 | `finish` | from the node, the changeset or the push, then the finish (Decision 3) | a refused push (the item failed), a refused finish |
| 7 | `release` (always) | the validation's pod, the session dir, the proxy, the bearer, the account, the scratch, the pod — what the steps held, last first, as the `defer`s did; a detached session's left for the node that adopts it | — |

Each record is logged once the job ends, `item <label>: step <n>
<name>: <outcome> (<took>)[: <err>]` (`StepLine`), a skipped step with
no time. `afterBuild` — an adopted build's tail — runs `run` (the end
read), `validate`, `finish` and `release` the same way, so an adopted
build logs its records too.

The `validate` step is the node's validator (ADR-014: its retry turn,
its attestation, its pod) under the step's name; `session.Validate`, the
job-container executor item 5 adds, is the form it takes once the retry
turn moves into the session package. The `pod` step is
`openOrgSessionPod`; `session.PodStep` is its org-free form.

### 2. The credential boundary is the job container's edge

Nothing the job container holds — its env, its HOME, the workspace and
git dirs it mounts — carries a GitHub credential or the session's
bearer.

- **The checkout.** A sandboxed build's `ItemCred` is env-only
  (`KeepOffCheckout`): the helper `NewItemCred` wrote is removed before
  the checkout, `PrepareWithRetry` fetches on `OnItemToken` (the token
  in `GIT_CONFIG_*` of each git command alone), and `Refresh`,
  `Reissue` and `ForPush` renew the token without writing it anywhere;
  `Reauthorize` (a rebuilt worktree) clears what it had to write. The
  checkout step reads back every git config of the workspace
  (`session.ConfigHolding`: its own, a worktree's `config.worktree`,
  the shared clone's) and fails if any names the token. An adopted
  sandboxed build's checkout is cleared the same way.
- **The env.** `sandbox.HeldOutEnv` (`GH_TOKEN`, `GITHUB_TOKEN`,
  `GH_ENTERPRISE_TOKEN`, `GITHUB_ENTERPRISE_TOKEN`, `GH_CONFIG_DIR`)
  never reaches a pod whatever the allow-list says (item 4), and a
  sandboxed build no longer makes the gh config dir or `SessionEnv` at
  all. A pod's `gh` is signed out; what the agent asked of GitHub it
  asks of the mesh tools.
- **The bearer.** The session's runsheet bearer stays on the node: the
  pod's relay reaches a node-side endpoint on a unix socket (item 3,
  `mcprelay.Serve`), and every MCP server goes through the session's
  proxy ([ADR-038, one proxy](adr-038-a-session-reaches-every-mcp-server-through-one-proxy.md)).

A host session (no sandbox) has no job container: it keeps the helper
and `GH_TOKEN`, as ADR-001 says, since it runs as the node's user and
could read the node's files anyway.

### 3. The finish is a changeset, from the node

`session.Finish` (item 6), on the node and never by exec in the pod:
the builder's changes staged in the worktree's own index, its tree
written, the objects that differ from the base tree (`origin/<base>`,
else `origin/<default>` when the base branch is gone, ADR-033) offered
to the platform's `changeset/missing`, only the missing uploaded, the
changeset recorded (`parent` the item's previous on a rework) and the
item finished naming `changesetId` — the platform makes the branch. The
attestation of the commit validated is uploaded first; the usage report
rides the finish as `FinishWorkItem`'s did. Logged `item <label>:
changeset <id> recorded (<n> path(s), <m> object(s) uploaded); finished
<branch> ($x)`.

The node takes that path only when `gateway.Client.TakesChangesets` —
the platform's `FinishWorkItemInput` takes `changesetId`, introspected
and kept `ScalingProbeEvery`. Otherwise — and on a platform that
answers the changeset routes 404 — the finish is the node's push on
the item's token (`pushBuild`, `OnItemToken`), the attestation of the
pushed head, and `FinishWorkItem` on it, as ADR-023 Decision 2 and
ADR-014's amendment have it; the log line is the old `pushed <sha>;
finished <branch>`.

## Consequences

- A build's history is its records: which step ended it and how long
  each took is one grep of node.log.
- A pod cannot push, fetch or call GitHub's API on the item's token,
  and cannot read the session's bearer. An agent that ran `git fetch`
  or `gh pr view` inside the pod now fails there; the prompts already
  send it to the mesh tools.
- On the changeset path no branch is pushed by the node; the platform's
  commit is not the one the node validated, so the attestation names
  the validated commit and the platform's half (ADR 058) decides how a
  changeset's tree is attested.
- `gatewaytest.Fake.Changesets` serves the changeset routes and a
  finish naming `changesetId` (`ChangesetsOf`, the item's
  `latestChangesetId`); off, it is the older platform.

## Tests

- `go test ./internal/session/... -run
  'TestStepsRunInOrder|TestWorkspaceLeavesNoCredential|TestJobContainerHoldsNoCredential|TestValidateRunsInTheJobContainer|TestFinishRecordsAChangeset|TestFinishNeverPushesFromTheJobContainer'`
  and `TestEndedStepSkipsTheRestButNotAlways`.
- `internal/node/session_steps_e2e_test.go`'s
  `TestBuildRunsAsSessionStepsE2E` (`-tags e2e`): the node's Git, a
  stand-in GitHub taking only the item's `ghs_` token, the Claude Code
  harness in a pod on `sandbox.Fake` and the fake gateway with
  changesets — seven records succeeded, neither `ghs_` nor the bearer
  in the job container's env or any file it mounts, a changeset whose
  tree is the agent's commit's, the item finished naming it, nothing
  pushed.
- `scripts/e2e-sandbox.sh` on podman: `TestProbeSessionE2E` greps the
  pod's env and files for `ghs_`; `TestSandboxedSessionsE2E`'s stub
  agent greps its container for `ghs_` and `mesh-session` and the build
  logs its seven records; step 2c runs `TestBuildRunsAsSessionStepsE2E`
  (docs/e2e-sandbox.md).
