---
summary: An item of an urgent or high goal whose Claude evaluation passes is evaluated again on Codex with the same prompt in the same workspace; both verdicts are recorded, labelled, in one EvaluateWorkItem, a Codex fail fails it, an unavailable or paced Codex keeps the first verdict with a line saying so, and the Codex account's limits (codex:<account>) never pace Claude. Builders on Codex come later, behind a per-project setting. A sandboxed Codex session runs in its pod, outlives the node and is adopted by the next process (amendment 2026-10-17).
status: accepted
---

# ADR-005: Codex as the evaluator's second opinion

Numbered with the initiative's other node decisions (goal
`codex-adapter`, item 3 names it so); `adr-005-accounts-on-the-node.md`
and `adr-005-capacity-across-accounts.md` are unchanged.

## Context

The initiative "Mesh runs anything, on any account" (Chris,
2026-09-25) has a node run several agents. Codex is the second
(`internal/harness/codex.go`, `docs/codex-events.md`): `codex exec
--json` in the workspace, a fresh `CODEX_HOME` per session, node.yaml's
`codex:` block mapping the Claude tiers to Codex models. Its first use
is where a second, independent judge is worth the most and costs the
least: the evaluation of work on the goals that matter most. A single
evaluator on one model shares that model's blind spots with the builder
that ran on it; a pass is what opens the pull request, so a wrong pass
is the expensive mistake. Nothing on the platform knows of a second
evaluator, and none of this should need it to.

## Decision

**When it runs.** The `Evaluator` has an optional `Second` harness
(Codex) and `SecondModel` (`CodexConfig.CodexModel` of node.yaml's
evaluate model). It runs when the item's goal is `urgent` or `high`
(`gateway.PriorityRank`), a `Second` is configured, and Claude's
verdict is a pass — with the same prompt, in the same workspace, on the
same item token and mesh-context. A first fail stands on its own; a normal
or low goal never runs Codex. `mesh node start` builds the Codex
harness only when node.yaml has a `codex:` block and its binary is
found, on the block's first account (none: the operator's own Codex
login); otherwise the node evaluates as before.

**How disagreement fails.** The second session is read with
`ParseVerdict`. Both pass: passed. Codex fails: failed — a disagreement
fails the evaluation, and the platform readies the item for its builder
as for any fail. Either way the notes carry both, labelled
`Claude (first evaluation): VERDICT …` and `Codex (second opinion):
VERDICT …`, truncated together to the 4000-character note (each half,
a shorter one leaving its room to the other) so neither is cut out, and
the models used on the last line. The cost recorded is the sum of both
sessions. Everything goes through the one `EvaluateWorkItem` call: no
platform contract changes.

**The fallback.** A second session that errors, times out or ends with
no verdict keeps the first verdict, with the line `Codex second opinion
unavailable: <reason>` in the notes, and the node logs it. Codex being
down, out of quota or misconfigured never holds an item in evaluating.
A cancelled node (shutdown, drain deadline) records nothing, as for any
evaluation, and the next tick evaluates again.

**The Codex account's limits are kept apart.** The Codex harness's
readings land in `Loop.Limits` under `codex:<account>`
(`CodexLimitsAccount`, through `ObserveLimitsAs`). `Limits.Readings` —
what the node paces by (`nodePace`, and the strictest reading when no
Claude accounts are configured), chooses accounts by (`ChooseAccount`)
and scales by — leaves them out; the heartbeat carries them. A Codex
account that is paced or stopped by the node's `limits:` thresholds
skips the second opinion, which the notes say as unavailable, and
nothing else: it never paces or stops a Claude session. Codex's exec
stream carries no plan limits today (`docs/codex-events.md`), so until
it does the key is empty and only its dollars count.

**Builders on Codex come later**, behind a per-project setting, once the
second opinion has shown how often Codex's judgement and Claude's agree
and what a first-pass rate on Codex would be.

## Consequences

- Urgent and high work pays for two evaluations on a pass, and a pass
  needs two judges; normal and low work is as before.
- A disagreement sends the item back to its builder with both reviews,
  which may occasionally cost a rebuild for a finding Claude would have
  waved through — the point of the second opinion.
- An unavailable Codex degrades to today's single evaluation, visibly in
  the notes; it never blocks.
- Codex has no budget flag: the second session is bounded by the
  session timeout, not `MaxBudgetUSD`, and its cost is known only from
  its tokens at list price (`CodexPrices`).
- Only the first `codex:` account is used; choosing among several Codex
  accounts by headroom waits for a platform-issued Codex credential.

## Amendment, 2026-09-28: limits from the session file, a session in a home the node prepared

Goal `evaluations-run-on-codex` (item 1) changes both things this ADR
said were missing.

**Codex's exec stream carries no plan limits today** stands — the exec
event schema still has none — but the session file under
`$CODEX_HOME/sessions/` does: `harness.ParseCodexSessionLimits(home)`
reads the newest one's last `rate_limits` snapshot and maps it onto
`harness.RateLimit` (`docs/codex-events.md` "Plan rate limits"), and
`Codex.Run` sets `Result.RateLimit` from it when the stream carried
none — every time, until the exec schema grows the field itself. A
Codex account's readings still land under `codex:<account>`
(`CodexLimitsAccount`) and still never pace or stop a Claude session;
what changes is that they are no longer always empty. The shape is
modelled on codex-cli 0.157.0's source, unverified on a live
ChatGPT-plan login as of this date (no plan login exists on the node
that verified it) — the first live plan run should confirm it and
update the doc's note.

**A session may run in a home the node prepared.** `Codex.prepareHome`
now honours `job.Env`'s own `CODEX_HOME=<dir>` when the caller sets
one: it writes only the MCP config into that directory, copies no login
from `Account`, and `close` neither removes the directory nor writes a
login back — the `AccountSession` that made it (an issued `openai`
credential landing an oauth login there, the goal's item 2) owns its
lifetime, not the harness. Without a `job.Env` `CODEX_HOME`, behaviour
is unchanged: a fresh temporary home per run, made and removed by the
harness as before.

## Amendment, 2026-09-28: the family is chosen per session

Goal `evaluations-run-on-codex` (item 3) changes **When it runs** and
**Builders on Codex come later**; the decision is
[ADR-011](adr-011-evaluations-run-on-the-family-with-headroom.md).

**When it runs**, now: an evaluation runs *first* on Codex when a Codex
account — an issued one, or node.yaml's `codex:` account — has headroom
for the goal's priority, else on Claude; a full `gpt-…` evaluate or
agent model pins Codex (the item waits in evaluating without headroom),
a `claude-…` one pins Claude. The second opinion on an urgent or high
pass runs on the *other* family — Claude after Codex — and the labels
name family and model: `Codex (first evaluation, gpt-6-sol): VERDICT:
PASS`, `Claude (second opinion, opus): VERDICT …`. The `Evaluator`'s
`Second`, `SecondModel` and `SecondAccount` are now `Codex`,
`CodexModel` and `CodexAccount`, beside `CodexAccounts`. `mesh node
start` builds the Codex harness whenever the binary is found, with or
without a `codex:` block; with neither a block nor an issued Codex
account the node evaluates as before. "Only the first `codex:` account
is used" still holds for node.yaml; issued Codex accounts are chosen
among by headroom.

**Builders on Codex come later** still holds for builders; evaluators
no longer wait for it.

## Amendment, 2026-09-28: the stream is kept 24 hours

Goal `codex-sessions-reach-the-context-tool` (item 2). A Claude
session's `output.jsonl` and `stderr.log` sit in
`sessionunit.Dir(job.Workspace)` until `harness.RemoveSession`; `Codex.Run`
tee'd both into a 64KB in-memory buffer alone, so once a session ended
nothing of what Codex said, or its own diagnostics, survived it — the
gap this initiative's statement found itself blocked by (Codex's
`prologue` calls failing with no evidence of why left on disk anywhere).

**`Codex.WorkRoot` keeps the whole stream.** Set (`node.WorkRoot()`, by
`codexSecondOpinion` in `internal/commands/nodecmd.go`), `Codex.Run`
writes the whole `codex exec --json` stream and stderr — secrets
redacted through the existing `redactWriter`, exactly as `c.Stderr`
is — to `output.jsonl` and `stderr.log` under
`<WorkRoot>/kept-sessions/<name>-<unix time>/`, beside a small
`state.json`-like note (`keptSessionNote`: kind, item/goal from
`job.Meta`, model, started at — never the prompt, never a secret).
`<name>` is the job's label up to its first space, slashes turned to
dashes (an evaluate job's `goalslug/seq evaluate` gives
`goalslug-seq`), so the goal, then a slug, then the seq item lands
readably in the directory name; the unix time keeps two sessions of
the same item apart. The directory lives outside
`sessionunit.Dir(job.Workspace)` entirely — a Codex session never had
one of those (it was not `Outlive`, adr-009; a sandboxed one is since the amendment of 2026-10-17 below) — so `harness.RemoveSession`
on the job's own work dir never touches it: nothing more had to change
there. `Result.KeptSessionDir` carries the path back to the node, empty
when `WorkRoot` is unset (every existing Codex test): the session's
stream then lives only in the 64KB buffer, exactly as before.

**`harness.KeepSessionOutput` is 24 hours**, and
`harness.SweepKeptSessions(root, now)` removes a kept directory once its
note's `startedAt` is older than that, leaving a younger one (a note
that cannot be read falls back to the directory's own modification
time). `Codex.Run` sweeps `WorkRoot` at the start of every session, so
the node need wire nothing into its tick for this alone — "each Codex
start" of the brief's two options, not "each tick".

**The node names the kept path when it has something to blame.** In
`internal/node/evaluate.go`, `afterEvaluate` appends
`" kept at <dir>"` (`keptAt`) to a Codex evaluation's session error and
its no-verdict error, and logs `"a tool errored kept at <dir>"`
(`hasToolError`, over `Result.Tools`) beside the existing served-tools
note when the verdict was read but some MCP server's calls — mesh-context's
included, unlike `SessionTools.UseNote`, which only ever names a
session's own `Use` tools — carried an error. A Claude session names
nothing here: `Result.KeptSessionDir` is empty for it.

A Codex evaluation's stream and stderr now outlive the process for a
day, so the next investigation into a Codex tooling failure has raw
evidence on disk instead of a guess; `kept-sessions` grows by one
directory per Codex session and is swept on the next Codex start, so a
node that stops running Codex sessions stops sweeping too and leaves an
old directory past its day — acceptable, since nothing runs Codex there
to make more. The 64KB `capBuffer` a Codex session's error paths already
read from is unchanged: `KeptSessionDir` is additional, not a
replacement.

## Amendment — 2026-10-02 (goal builtin-is-mesh-context)

The built-in attachment is `mesh-context` (`tools.Builtin`, mesh-cli
#264/#396); `prologue` is the deprecated alias read until 2026-10-28,
not the name a current session carries. **When it runs** said "the
same item token and Prologue"; the served-tools note above said "some
MCP server's calls — Prologue's included". Both named the built-in by
its old name after the rename had already reached the node's own
attachment (`adr-006`'s 2026-09-28 amendment); both now say
`mesh-context`. Nothing about when the second opinion runs or what the
note names changes — only the word.

## Amendment — 2026-10-02: no Codex session on node.yaml's account (goal a-session-never-runs-on-the-machines-own-login)

The Decision's "on node.yaml's first `codex:` account" and **When it
runs** as amended on 2026-09-28 ("an issued one, or node.yaml's
`codex:` account") no longer hold: a Codex session — first evaluation or
second opinion — runs only on an issued OpenAI credential serving
`codex` with headroom, opened by `Credentials.Open` (adapter codex);
none is Claude for an unpinned evaluation, a wait for a pinned one, and
an unavailable second opinion. The Evaluator's `CodexAccount` (the
harness's own account's Limits key) is gone; "Only the first `codex:`
account is used" is moot, since node.yaml names none. The operator's
`~/.codex` and `$CODEX_HOME` are never read: the harness refuses a job
with no placed home (`codex: no issued credential on the session;
refusing the operator's own login`). The decision is
[ADR-011's amendment 2026-10-02](adr-011-evaluations-run-on-the-family-with-headroom.md).

## Amendment — 2026-10-02: one evaluator for ordinary work (goal one-evaluator-for-ordinary-work)

This replaces the Decision's "When it runs": a first fail no longer
always stands on its own, and a normal or low goal's item may get a
second opinion. The same text is in ADR 011's amendment of this date.

Evaluation cost as much as building (15,564 evaluate minutes against
17,629 build minutes in a week), and a second judge is worth its cost
only where a wrong verdict is expensive. The second opinion is now asked
by a rule, not by priority on a pass alone. `secondOpinionWanted(evalDecision)` in
`internal/node/evaluate.go` is a pure decision returning whether
and why; the first reason that holds is the one recorded:

| The item | Evaluators | Reason |
|---|---|---|
| its goal is `urgent` or `high` | 2 | `urgent/high goal` |
| its `protectedFiles` is non-empty, or a file its diff changed (`Workspacer.ChangedPaths`) matches a glob of its `protectedRulePaths`, the project's `protectedPaths` or the `protected_paths:` list of the checkout's `.mesh/config.yml` (`globMatch`: `path.Match` per segment, `**` any number of segments) | 2 | `protected path <path>` |
| its first verdict is FAIL with at least one failed clause that is not a reading of the host (`hostReadingClause`, the planner's lexical test behind `hostReadingAcceptance`, on the clause and its SATISFY; ADR-020) | 2 | `first verdict FAIL on a correctness clause` |
| the checkout's `.mesh/config.yml` says `second_opinion: always` | 2 | `second_opinion: always` |
| anything else — a normal or low goal touching no protected path, a first FAIL on host readings alone | 1 | `normal/low goal, no protected path` |

**The verdict on a first FAIL.** The second verdict decides, as it did
on a first PASS: a second FAIL is failed with the union of both
sessions' failed clauses (the first's, then the second's not already
named), and a second PASS passes the item, both reviews in the notes. A
wrong fail costs a rebuild, the most expensive thing the evaluator can
spend, so a second judge that finds the acceptance held is let to
overturn it. On a first PASS nothing changed: both pass is passed, a
second fail is failed.

**The setting.** `second_opinion: always|rule` is read from the top
level of the item's repo checkout's `.mesh/config.yml`
(`readCheckoutConfig`, which reads its `protected_paths:` globs too —
the repo's own protected paths beside the project's); absent, unreadable or any other value is
`rule`. It belongs on the project, read at claim time through
ProjectIndex, and waits for the platform to serve that field: nothing
was added to `schema.json`, and the reader carries a TODO naming it.

**The record.** The choice is one line in the notes before the
`(evaluated on …)` tail and one in node.log, `item <label>: evaluators:
one — normal/low goal, no protected path` or `evaluators: two — <reason>`
(`evaluators: one — <reason>, but no other family on this node` when the
rule wants two and the node has no second family). Every
`evaluateWorkItem` — verdict or no verdict — carries `evaluatedModel`,
the model of the evaluation whose verdict came first
(`gateway.EvaluateChoice`). What already degrades to one verdict still
does, with `<Family> second opinion unavailable: <why>` beside the
`evaluators: two` line: an adopted session, a family blind on the first
attempt, a failed preflight, no Codex headroom.

**The retry.** A FAIL the second family confirms or contests is recorded
for the retry as any FAIL is: both reviews in the one `evaluateWorkItem`'s
notes and both sessions' clause lists in its `failedClauses` (the
union), so the next attempt's evaluator is given them and `SameClause`
sees a clause either family named. The verdict rule itself (TRD §3: a
disagreement after a first PASS fails) is unchanged; only when the
second opinion is asked is. A first attempt that reached no verdict —
blind, or no `VERDICT:` line — is never a first FAIL: those paths are
unchanged and ask no second opinion.

## Amendment 2026-10-17: a sandboxed Codex session outlives the node and is adopted (goal codex-sessions-run-in-the-sandbox)

The 2026-09-28 amendment *the stream is kept 24 hours* said a Codex session is not `Outlive`: it ran attached on the
host and ended with the node, so an upgrade mid-evaluation lost it and
the next process evaluated the item again. Since this goal a Codex
session on a node whose sandbox is on runs in its own pod from the node
image (ADR-007's amendment of this date), and that pod is its unit as
it is a sandboxed Claude session's (ADR-008, ADR-009, ADR-007's
amendment 2026-10-13). **"A Codex session ends with the node" is
retired for sandboxed sessions:**

- A Codex evaluation on a sandboxed node is `Job.Outlive`, and
  `Codex.DetachSandboxed` (set by `mesh node start` with a sandbox, as
  `ClaudeCode.DetachSandboxed` is) runs it detached — `codex exec` by
  `Session.StartDetached`, `podman run -d` named `sandbox.AgentName(pod)`,
  its prompt in `<workdir>.session/.prompt`, its streams in
  `output.jsonl` and `stderr.log` (`Codex.runDetached`,
  `internal/harness/codexdetach.go`).
- Its `state.json` names `harness: codex`, `runner: podman`, the pod,
  container and HOME, the runsheet token file, and the login's
  `loginFile` and `loginCache` (`SessionMeta.LoginCache`, the
  credential's `CodexLoginCache`) — paths only, never a secret.
- A restart (`Loop.Restart`, a self-update, `mesh node upgrade`)
  detaches it: the pod, HOME, token file and login stay. The next
  process's `FindSessionsIn` finds it, `SweepSandbox` keeps it, and
  `Evaluator.AdoptEvaluation` routes it by the state's adapter to
  `Codex.Attach`, which follows the container from the saved offset,
  parses the Codex stream, writes a refreshed `auth.json` back to the
  login cache and removes the pod and HOME; the verdict goes through
  the same `afterEvaluate` as a live session's, recorded exactly once
  (`TestCodexPodSessionAdopted`, which fails with the Codex branch of
  `AdoptEvaluation` removed).
- What still ends with the node: a host Codex session (sandbox off),
  and the second opinion, whose job is copied before `Outlive` is set,
  so it runs attached in the pod and a restart mid-second-opinion leaves
  the first verdict to be evaluated again by the next process.
