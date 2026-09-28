---
summary: An item of an urgent or high goal whose Claude evaluation passes is evaluated again on Codex with the same prompt in the same workspace; both verdicts are recorded, labelled, in one EvaluateWorkItem, a Codex fail fails it, an unavailable or paced Codex keeps the first verdict with a line saying so, and the Codex account's limits (codex:<account>) never pace Claude. Builders on Codex come later, behind a per-project setting.
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
same item token and Prologue. A first fail stands on its own; a normal
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
name family and model: `Codex (first evaluation, gpt-6-astra): VERDICT:
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
