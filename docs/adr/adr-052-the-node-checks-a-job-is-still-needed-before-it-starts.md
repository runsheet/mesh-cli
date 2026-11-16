---
summary: A v1 brief may carry preconditions — {type ref_head, repo, ref, sha} or {type pr_head, repo, number, sha}. After the lease's git grant opens and before its checkout, pod and session, the node reads each ref with git ls-remote through that grant; when the code host answers that a ref has moved or is gone, the node ends the lease with POST /v1/leases/{id}/no-longer-needed and the reason, which is neither a hand-back nor a failure. A precondition the node cannot tell (unknown type, a repository the grant does not reach, a failed read) is logged and the job runs. Amended 2026-11-16: a validate lease whose checkout finds its base_ref gone (git cannot find the remote ref, and ls-remote answers HEAD but not the ref) ends no_longer_needed too; other kinds still hand it back cannot_run.
status: accepted
---

# ADR-052: The node checks a job is still needed before it starts

2026-11-15. Mesh v1. Applies to `internal/nodev1` (needed.go, lease.go),
`internal/meshapi` (types.go, client.go) and `internal/actor`
(actor.go). The platform side is projectmesh-io/api's ADR 001
amendment, "a job is still needed or ends quietly". runsheet goal
`a-job-is-still-needed-or-ends-quietly`.

## Context

On 2026-11-15, in one hour, runsheet offered 145 merge-candidate
validations. 58 were cancelled within about two minutes because a newer
candidate replaced them, and 5 finished. Each validation holds up to 24
CPUs on the one node. A validation that starts on a head that has
already moved does a checkout, starts a pod and its services, and runs
suites, all for a result nobody reads, while the live candidate waits.

A cancel from the product races the take. The node is the last place
that can tell, cheaply, whether the work is still wanted: it is about
to read the repository anyway.

## Decision

**The brief says what must still be true.** `meshapi.Brief.Preconditions`
is a list of:

- `{type: "ref_head", repo, ref, sha}`: `repo`'s `ref` must still point
  at `sha`. A bare branch name is `refs/heads/<name>`.
- `{type: "pr_head", repo, number, sha}`: the head of `repo`'s pull
  request `number` must still be `sha`.

**The node checks them before the lease's work starts.** In `work()`,
right after the lease's git grant opens and before the checkout
(`Runtime.notNeeded`, needed.go). That is before the lease's pod, its
services and its session. With the sandbox (`--sandbox`) the pod is
made after the checkout, so nothing of the pod exists yet.

**It reads the code host as the checkout does.** One
`git ls-remote <grant base>/<repo>.git <ref>` per precondition, run
under `githarden` in the lease's own directory. It goes through the
lease's grant on the node's git relay, on the read token the relay
mints for the lease's session. There is no new credential and no call
to the vendor's API:

- A `ref_head` is read at its ref.
- A `pr_head` is read at the vendor's change ref
  (`nodetools.ChangeRef`): `refs/pull/<n>/head` on GitHub,
  `refs/merge-requests/<n>/head` on GitLab.
- The ref advertisement always comes from upstream, never the node's
  mirror (gitrelay/mirror.go), so the answer is the vendor's now.
- The whole check is bounded at 30 seconds (`neededWithin`).

**A precondition that no longer holds ends the job quietly.** When the
code host answers and the ref is at another commit, or is gone,
`work()` returns `outcome{notNeeded: reason}` and `report()` calls
`meshapi.Client.NoLongerNeeded` (`POST /v1/leases/{id}/no-longer-needed
{reason}`). Mesh ends the job `no_longer_needed`:

- It is not a failure: the node calls neither `fail` nor `finish`.
- It is not a hand-back: the node calls no `hand-back`, so Mesh counts
  none and no breaker hears of it.
- The actor's end is `actor.ReportNoLongerNeeded`, with the reason.
- It wins over every other end but a lease that is already gone: a
  node that is stopping still ends the job rather than hand it back
  `node_restart` to be taken and checked again.
- The reason names what moved, for example `the head of pull request
  acme/widgets#12 is now <sha>, not <sha>`.

The grant then closes and its token is revoked, as on every end, and
the lease's directory is removed. No session ran, so there is no usage.

**Only an answer ends a job.** The job runs, as it would have without
the precondition, and the node logs why, when:

- the type is one this node does not know;
- the precondition's repository is neither the grant's own nor one of
  its read repos (with a relay sidecar the node's own grant reads the
  job's repository alone);
- the vendor keeps no ref for a change's head (Bitbucket);
- the brief names no git at all;
- the read fails or times out.

Failing open is deliberate. A precondition saves work; it is not a
guard the product may rely on for correctness. The product's own
supersede and cancel are unchanged.

## Consequences

- A validation whose head moved costs one token mint and one ref read,
  not a checkout, a pod and a run.
- The check is made once, before the work. A head that moves while
  the job runs is still the product's to cancel.
- A Mesh older than the api amendment sends no preconditions, so this
  node behaves as before against it.
- `internal/meshapi/testdata` gains `take-preconditions.json` and
  `lease-no-longer-needed.json`, copied from projectmesh-io/api's
  `contracts/v1`, and `TestClientSpeaksTheContract` replays both.

## Tests

`internal/nodev1/needed_test.go`, on the package's fakes (a fake Mesh,
a fake Latchkey and git http-backend as the code host):

- `TestAJobWhosePullRequestHeadMovedEndsNoLongerNeeded`: with and
  without job actors, the lease ends `no-longer-needed` with the
  reason; Mesh hears no `fail`, `hand-back` or `finish`; the session
  never starts; the read used the lease's minted token, which is
  revoked.
- `TestAJobWhoseRefMovedOrIsGoneEndsNoLongerNeeded`.
- `TestAJobWhosePreconditionsHoldOrCannotBeToldRuns`.
- `TestPreconditionRef`.

## Amendment 2026-11-16: a validation whose ref is gone at checkout ends quietly

**Context.** On 16 Nov dl360 handed back 98 leases whose checkout
failed with `git fetch: exit status 128: fatal: couldn't find remote
ref …`, every one of them cannot_run, every one queued again by Mesh:

- runsheet job `1c9c18ff`, 78 times: a head validation of
  `mesh/test-reliability-2026-w46-34`. Its build finished on its own
  base (it committed nothing), so the branch was never pushed, and the
  validation's brief named that branch with no precondition;
- latchkey job `c1f2d21f`, 18 times: a **rebase** onto
  `mesh/a-tenant-connects-a-vendor-by-oauth-consent-1`, a dependency's
  branch GitHub deleted when its pull request merged (latchkey #830,
  21:15 AEDT).

A precondition (above) would have caught the first only if the offer
named one; runsheet now names it (runsheet goal
`a-job-is-still-needed-or-ends-quietly`, and its 16 Nov follow-up). This
amendment is the node's backstop for an offer that names none.

**Decision.** When a lease of the validate kind (`validationKind`:
`validate`, bare or under its product) fetches its brief's `base_ref`
and git answers that it `couldn't find remote ref`, the node reads the
ref again with `git ls-remote origin HEAD <ref>` in the checkout,
through the same grant and relay (`refGone`, checkout.go). The ref is
**gone** only when that read succeeds, names the repository's `HEAD` —
so the code host answered and advertises its refs — and names no ref
matching `<ref>`. The lease then ends `no_longer_needed` with the reason
`<repo> <ref> is gone from the code host, so there is nothing left to
validate` (lease.go), exactly as a precondition that no longer holds
does: no hand-back, no failure, its session never started.

Everything else is unchanged:

- any other kind (build, rebase, evaluate, …) whose base ref is gone is
  handed back `cannot_run` with the fetch's own words, as before. A
  rebase onto a merged dependency's deleted branch needs the product to
  re-target it; ending it quietly would only have runsheet ask for the
  same rebase again;
- a fetch that failed any other way, an ls-remote that fails, one that
  advertises no `HEAD` (an empty answer), and one that finds the ref
  after all (pushed since the fetch) keep the fetch's error and its
  cannot_run;
- a git token that was not minted is still `credential_timeout` first.

**Risk.** The one way this misfires is a validation ended quietly whose
ref was in fact there. That needs git to say the remote ref is missing
*and* a second read, moments later, to answer with `HEAD` but without
the ref: a relay or code host blip makes either read fail, which keeps
cannot_run. A validation ended in error costs the product one re-offer
(runsheet treats a validate job that ended `no_longer_needed` as its
head gone and does not offer it again on its own). It never turns a red
into a green: no verdict is reported.

**Tests.** `internal/nodev1/refgone_test.go`: `TestRefGone` (a local
bare repository: a missing ref by short and full name is gone; a
present one, a fetch that failed another way, an origin that does not
answer and an empty origin are not);
`TestAValidationWhoseRefIsGoneEndsNoLongerNeeded` (with and without job
actors: one no-longer-needed end naming the ref, no hand-back, fail or
finish, the session never ran);
`TestAnotherKindWhoseRefIsGoneIsHandedBackAsBefore` (rebase, build and
another kind: cannot_run with the fetch's own words).
