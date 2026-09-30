---
summary: After a build session commits and before the push, the node runs the repository's `.runsheet/validate.yml` on HEAD — the steps the branch's diff selects plus the always ones — in the session's pod (runner image by digest, sandboxed) or on the host (`host:<hostname>`, unsandboxed, the session's env less its secrets), writes the attestation to `<workdir>.session/attestation.json` and uploads it on the item's token after the push; a failing step gets the builder one retry turn, then fails the item `validation failed: step <name> exited <code>`, and every step's time rides the usage record's `validation`.
status: accepted
---

# ADR-014: A build validates before it pushes

2026-09-28, goal `node-attests-its-validation` (item 3). Amends the
session's end (mesh-specs trd §3) as this node implements it, and
[ADR-007](adr-007-the-sandbox-on-the-node.md) (where the steps run).

## Context

runsheet ADR 032 has the platform sign a validation attestation for an
item's head commit, so the PR workflow can skip the CI the node already
ran. [ADR-013](adr-013-validation-runs-from-a-declared-config.md) is
the config, selection, run and attestation (`internal/validate`); #318
is the upload (`gateway.RecordValidationAttestation`) and the usage
report's `validation`. This is where the node calls them.

## Decision

**When.** In `Runner.afterBuild`, after a successful session that is
not `ALREADY DONE`, and before `Push`/`FinishWorkItem`. A repository
with no `.runsheet/validate.yml` logs `no validation declared` once per
build and finishes as before. An unreadable one fails the item
(`validation: …`): the builder may have broken it.

**What.** `Workspacer.ChangedPaths` (`git diff --name-only
origin/<base>...HEAD`, beside `Diff`) selects the steps
(`validate.Select`), re-read on each run, so a retry's commit selects
its own. They run on HEAD; the attestation's `commit` is that HEAD, and
the node logs it if the push answers another.

**Where.** With a sandbox (`Runner.Sandbox`) each step is one
`sandbox.Session.Run` (`sandboxRunner`) in the build session's own
session — a fresh pod of it, the agent's having gone, with the
workspace mounted and the job's allow-listed env; an adopted session
gets a session of its own. A limit kill is the step's failure (124 for
the runtime's timeout, else 137), not the node's condition. The
attestation's `runnerImage` is the node image's digest
(`sandbox.ImageDigest`: `podman image inspect --format {{.Digest}}`),
else its ref, and `sandboxed: true`. Services started for the agent
went with its pod: a step that needs them declares them again through
the step's own command until the sidecars outlive the agent. With no
sandbox (`sandbox: off`, the mini today) `validate.HostRunner` runs in
the checkout with the session's environment less every variable that
carries one of its secrets and less what the job unsets;
`runnerImage: host:<hostname>`, `sandboxed: false`.

**The attestation** (`validate.Build`: node id, the session id, the
item id) is written to `<workdir>.session/attestation.json` and, after
the push — so the platform signs for a commit that exists — uploaded by
`RecordValidationAttestation` on the node's own token, as every other
command. Not the item's `ghs_*` token: the gateway takes a Latchkey
machine token or an `rs_*` console token and checks that the caller is
the node holding the item; a GitHub token is refused outright, which is
what every upload up to v0.2.208 got back (the item token client,
`WithItemToken`, went with that fix). A platform without the endpoint is said
once per run (`ValidationNotices`); any other error is a progress note
on the item. Either keeps a copy at `<workdir>.attestation.json`, which
the session directory's removal leaves. Artifacts go to the gateway as
`<path>@sha256:<digest>`.

**A failing step** stops the run (the first failure) and gets the
builder **one** more turn, with `harness.ValidationRetryPrompt`: the
step, its command, exit code and the end of its output, and ADR 012's
hand-back rule — fix it and commit, or commit what works and hand
back; never push. A host Claude session whose id is known is resumed
(`Job.Resume`, the hand-back's own mechanism) on the budget left; a
sandboxed or Codex one, or one with no id, is a fresh session with the
failure after its brief. The turn has no hand-back of its own and does
not outlive the node (`Outlive` false): a restart during it leaves the
item to the platform's stall window rather than letting an adoption
grant a second retry. An adopted build gets no retry turn. The new HEAD
is validated again; a step still failing, or a turn that landed no new
commit, fails the item with `validation failed: step <name> exited
<code>` — the step's name first — reported on the item as well, and
the failed run's attestation is still uploaded. Nothing is pushed.

**Usage.** The usage report's `validation` is every step's
milliseconds summed over both runs, with its last exit code, on
`FinishWorkItem`; on the failure path `FailWorkItem` carries no usage,
so `SessionAccount.Finish` logs the report with it. A platform whose
report takes no `validation` (`ErrNoValidationUsage`) gets the finish
without it, said once per run. The retry turn's tokens and cost are
added to the build's report and cost.

**Time.** The validation counts against the job's timeout when it has
one (a deadline at the session's start plus it); a step's own timeout
is validate.yml's.

## Consequences

- An item cannot finish on a commit whose declared validation fails:
  the evaluator sees only commits that passed it.
- A build costs its steps' wall time before the push; `validation`
  minutes are the scorecard's "validation minutes spent".
- Until the platform issues a gateway token per item, the item's
  GitHub token is the bearer the upload carries (#318's assumption);
  the platform side confirms or corrects it.
- Until Podman is on the mini, its attestations say `host:<hostname>`.
