---
summary: After a build session commits and before the push, the node runs the repository's `.runsheet/validate.yml` on HEAD — the steps the branch's diff selects plus the always ones — in the session's pod (runner image by digest, sandboxed) or on the host (`host:<hostname>`, unsandboxed, the session's env less its secrets), writes the attestation to `<workdir>.session/attestation.json` and uploads it on the node's own token after the push, as `recordValidationAttestation(workItemId, input)` in the platform's shape (`specs/contracts/recordValidationAttestation.json`, introspected, no REST path); a failing step gets the builder one retry turn, then fails the item `validation failed: step <name> exited <code>`, and every step's time rides the usage record's `validation`. Amended 2026-10-09: after the last push of a build, rework or rebase the node attests exactly the pushed sha (validated again if it is not the one validated) before it reports finished, answers a `not the head (GitHub has X)` refusal once by validating and attesting X, retries a 429/5xx/transport error for 2 minutes, never fails an item for an attestation, and counts recorded and refused for the hour — pairing with runsheet goal `an-attestation-is-about-a-commit-not-an-item-state`.
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
once per run (`ValidationNotices`); a 429 or 5xx answer retries with
backoff, honouring a `Retry-After` header, and only then is it, or any
other error, a progress note on the item. Either keeps a copy at `<workdir>.attestation.json`, which
the session directory's removal leaves. Artifacts go to the gateway as
`<path>@sha256:<digest>`.

*Amended 2026-10-03 (goal the-nodes-attestation-reaches-the-platform,
item 1).* Every upload until now went nowhere: the node sent
`recordValidationAttestation(input: RecordValidationAttestationInput!)`
with `{aggregateId, namespace, attestation}`, a type the platform never
had, then a REST path the platform never had either, and said "the
platform takes no attestations yet". The call is now the
platform's own (runsheet ADR 032, As built (items 3 and 33)):
`mutation($id: UUID!, $in: Map!) { recordValidationAttestation(workItemId: $id, input: $in) }`,
no selection set (the answer is a Map, the platform's receipt), with
`in` `{commit, treeHash, runnerImage, suiteHash, sessionId, testsRun,
testsPassed, testsFailed, artifactDigests, at, commands [{suite,
command, exitCode, durationMs}], reports [{suite, content}]}`
(`gateway.ValidationAttestation`, converted by `gatewayAttestation`: one
command per step, the tests summed over the steps, `suiteHash` the
validate.yml's hash, a report per step whose results it parsed —
`validate.StepResult.Report`, the raw file or stdout, up to 256 KiB).
*Amended again by item 4:* `commands` names exactly the suites the run
ran, `{suite, command, exitCode, durationMs}` each, in the file's
order — possibly none: a diff no suite's paths match still uploads one
attestation with `commands: []`, so the verify step never answers
`missing` for "nothing affected"; `reports` is `[{suite, content}]`,
each the step's results file, stdout or `reports:` files, 256 KiB in
all over the run; and `suiteHash` is `validate.SuiteHash`, the
platform's canonical `validatespec.SuiteHash` of the file (ADR-013's
amendment), which the verify step compares with the base's.
The node id, the item id, the tree's old name, `sandboxed` and the
per-step rows stay on the local attestation: the platform takes the
holder from the bearer and the item from `workItemId`. The contract is
`specs/contracts/recordValidationAttestation.json`, copied byte for
byte from runsheet's (served at
`https://api.runsheet.dev/contracts/recordValidationAttestation.json`),
and `gateway`'s tests round-trip its input through `gatewaytest`.
Whether the platform has the mutation is an introspection probe,
`{ __type(name: "Mutation") { fields { name args { name } } } }`, asked
once and kept for `ScalingProbeEvery` (`takesAttestations`): only a
Mutation without the field is `Unsupported` — the once-per-run "takes
no attestations yet" — and nothing is sent. There is no REST path: none
exists on the platform.

*Amended 2026-10-03 (goal the-nodes-attestation-reaches-the-platform,
item 3).* An upload lands in one of three outcomes, in `uploadAttestation`.
Uploaded: the platform's considered answer, logged and done. Absent:
`IsUnsupported` — checked first, since such an error is also a
refusal — is the once-per-run "takes no attestations yet" alone, never a
note on the item; this is reserved for an *observed* absence, the probe
answering with no `recordValidationAttestation` field — a probe that
cannot be answered at all (the introspection query itself fails, a
transport error or a 5xx) is not cached and reaches `uploadAttestation`
as its own error, never read as the mutation being missing. Refused: a
platform that has the mutation and refuses it (`IsRefusal`), in its own
words — "<sha> is not the head of <branch> (GitHub has <sha>)", a tree
mismatch, the item held by no node, the App not installed — is a note
on the item, `validation attestation refused by the platform: <words>`,
verbatim, and the same words logged; never unsupported. A 429 or 5xx
answer retries with backoff, honouring a `Retry-After` header
(`retryOnTransient`, `AttestationBackoff`); spent, it is wrapped `after
N retries: <last answer>` and lands as any other failure would —
`validation attestation: upload failed: <error>`, a note on the item,
never the item's own failure. In every case — uploaded, absent, refused
or failed — a copy stays at `<workdir>.attestation.json`.

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

**After a rebase** (2026-10-03, goal
`the-nodes-attestation-reaches-the-platform`, item 5). A rebase
session (runsheet ADR 008) pushes a new head, and runsheet ADR 032
carries no attestation forward to it: *The honesty check*, rule 1 of
its item 32 amendment, says "a head the pull request had before a
rebase never reached main", and decision 1 signs only "the item's
branch head as GitHub reports it" — so the build's attestation names a
head the pull request no longer has, and the pull request's `verify
attestation` would answer `missing`. ADR 032 names no platform-side
carry-forward for an unchanged tree; the node's half is to attest the
rebased head. In `Rebaser.afterRebase`, once the push is read back
(`PushedHead`) and before `FinishWorkItemRebase`, `validateRebased`
runs the checkout's `.runsheet/validate.yml` exactly as a build's —
the steps the branch's diff against its base selects plus the always
ones, on that head (checked out detached if the checkout is anywhere
else), in the session's pod or on the host, by the same
`validator.start`/`run`/`upload` a build uses — writes the attestation
beside the rebase's workdir and uploads it on the node's own token.
Nothing in it fails the rebase, whose job is the head: a failing step
is logged `rebase <label>: validation step <name> exited <code>`, noted
on the item, and its run still uploaded for the platform to refuse or
record; a refused upload is a note on the item and a platform without
the mutation the once-per-run notice (shared with the Runner,
`ValidationNotices`); a validation that cannot run at all is a note
and no attestation. There is no retry turn and no `validation` usage
on the rebase's report. A repository with no validate.yml logs `no
validation declared` once and finishes as before.

**The head it pushed** (amended 2026-10-09, goal
`the-node-attests-the-head-it-pushed`, items 5–8; pairs with runsheet
goal `an-attestation-is-about-a-commit-not-an-item-state`, the gateway's
acceptance change). Until this amendment no attestation was ever
recorded: the node validated, attested, pushed once more and reported
the build finished, so the gateway (runsheet gateway/attestation.go,
the holder rule) saw a sha that was no longer the branch's head, or an
item already in `evaluating` or `review`, and refused every one. The
order is now, for a build, a rework (a build of an item that failed
its evaluation, the same `Runner.afterBuild`) and a rebase alike:

1. **the final push** — `Workspacer.Push` for a build or rework, the
   rebase's `--force-with-lease` read back by `PushedHead`;
2. **validate the pushed sha if it differs from the validated one**
   (`attestPushed`): the last run's commit is not the push's — the
   push landed another sha, or nothing ran yet, as after a rebase — so
   the pushed sha is checked out and validated first, logged
   `validation ran on <a>, the push is <b>; validating <b>`;
3. **attest that exact sha with its tree hash**: the attestation's
   `commit` is the pushed head and `treeHash` that commit's tree, never
   a commit validated before a later push;
4. **finish** — `FinishWorkItem` (build, rework) or
   `FinishWorkItemRebase` — only once the upload has completed,
   recorded, refused, unsupported or failed, so the item is still this
   node's while the platform signs.

**The newer-head answer, once.** A refusal naming another head, `<sha>
is not the head of <branch> (GitHub has X)` — another push landed on the
branch after this one — is answered once (`answerNewerHead`): fetch X
under the per-repo fetch lock, validate X exactly as the pushed head
was, attest X. A head that cannot be fetched, a validation that fails
or cannot run on it, or a second refusal of any kind is logged
`attestation refused <reason>` and the session finishes as it would
have; it is never asked again, and the checkout goes back where it was.

**The retry.** An upload that meets a 429, a 5xx or a transport error
is retried with backoff (`retryOnTransient`) for
`AttestationRetryFor` = 2 minutes from the first attempt, waiting 2s,
5s, 10s, 20s, 30s (`AttestationBackoff`), the last repeated, a
`Retry-After` header honoured and capped at the last; once the two
minutes are spent the last answer is logged and the session moves on.
A refusal — the gateway's considered answer — is not retried, nor is
an unsupported mutation. **An attestation never fails an item**: every
outcome, a refusal and a spent retry included, is a log line and a
note on the item, and the finish follows.

**The log lines** say what the gateway recorded:
`attestation recorded <sha> <suites> <status>` — the short sha, the
suites comma-joined (`no suite affected` for none) and the receipt's
status — or `attestation refused <reason>`, the gateway's words
verbatim (a newer head's refusals prefixed `newer head <X>:`).

**The hourly counts.** Every `attestation recorded` and `attestation
refused` is counted over the trailing hour (`AttestationCounts`, shared
by every session through `ValidationNotices.Counts`, kept at
`~/.mesh/node.attestations` so a restart inside the hour counts on).
The node's health line, logged on the first tick and every 15 minutes
(`DefaultHealthEvery`), ends `attestations recorded <n> refused <m>
(1h)` — `health: 1 of 2 running, attestations recorded 2 refused 1
(1h)` — and `mesh node status` prints `attestations: <n> recorded, <m>
refused (last hour)`. A run of refusals with no recorded beside it is
the sign the gateway's half, `an-attestation-is-about-a-commit-not-an-item-state`,
is not deployed.

## Consequences

- An item cannot finish on a commit whose declared validation fails:
  the evaluator sees only commits that passed it.
- A build costs its steps' wall time before the push; `validation`
  minutes are the scorecard's "validation minutes spent".
- Until Podman is on the mini, its attestations say `host:<hostname>`.
