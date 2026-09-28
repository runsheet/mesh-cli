---
summary: Before an evaluation starts the node chooses its family per session — Codex when a Codex account (issued or node.yaml's) has headroom for the goal's priority, else Claude; a full gpt-… id pins Codex and waits in evaluating without headroom, a claude-… id pins Claude; the urgent/high second opinion runs on the other family, labelled by family and model; the one usage report names adapter codex and the gpt-6 model of a Codex evaluation; before any Codex session starts the node preflights the built-in context server in the session's own home and env (`codex mcp get` and a tools/list handshake), and a failure moves an unpinned evaluation to Claude, keeps a pinned one waiting and makes a Codex second opinion unavailable.
status: accepted
---

# ADR-011: Evaluations run on the family with headroom

2026-09-28, goal `evaluations-run-on-codex` (item 3). The node half of
mesh-specs ADR 011 (`adr-011-evaluations-run-on-the-family-with-headroom`);
amends `adr-005-codex-second-opinion.md` ("When it runs", "Builders on
Codex come later").

## Context

`adr-005-codex-second-opinion` made Codex a second judge of urgent and
high passes, on node.yaml's first `codex:` account. Chris is buying
ChatGPT Pro so Codex (GPT-6 Astra, Sol, Luna) can carry the evaluations
themselves: last week 499 evaluation sessions, $484 and 16% of spend
ran on Claude. Item 2 of the goal made an issued OpenAI credential
serving `codex` a Codex account (`adr-005-accounts-on-the-node`,
amendment): `Credentials.Open` by adapter, the login in the session's
`CODEX_HOME`, its readings under `codex:<label>` (`CodexReadings`),
never pacing Claude. What was missing is the choice.

## Decision

**The family is chosen per session, by headroom.** Before an
evaluation prepares anything, the `Evaluator` judges the Codex accounts
the loop knows — `Loop.CodexCandidates`: the issued Codex accounts
`Known` holds, then node.yaml's `codex:` account (`CodexOwnAccount`, no
ID; left out when the block is) — with `ChooseAccount` over
`Limits.CodexReadings` and the node's `limits:` thresholds, for the
item's goal priority: a paced account takes only urgent and high work,
a stopped one nothing, an api key only what its priority allows. One
with headroom makes the session a Codex session: the Codex harness, on
`CodexConfig.CodexModel` of the item's Claude evaluate model
(`EvaluateModel`), whose defaults are `gpt-6-sol` for opus and
`gpt-6-luna` for sonnet and haiku when node.yaml names none (amendment
below). An issued account is opened by `Credentials.Open` with adapter
codex under the evaluator's own rule (`withChooser`); node.yaml's by
`Credentials.OpenOwn`, which still asks the claim-time read for the
session's tools but runs on the harness's own login. None with
headroom: the session runs on Claude exactly as before, and if the
chosen issued credential is gone by the claim-time read, it falls back
to Claude too. A Codex session is not `Outlive` (Codex still ends with
the node, `adr-009`).

**A pin wins over the choice.** `EvaluatorPin` reads the item's
`evaluateModel`, then its own or its agent's model (`ItemModel`,
`gw.AgentModel`): a full `gpt-…` id pins Codex and is the session's
model; a full `claude-…` id pins Claude. A bare tier (`haiku`, `sonnet`,
`opus`) pins nothing — the platform and the planner name one for nearly
every item, so treating it as a pin would put every evaluation on
Claude; it is a strength, mapped to Codex's tier. mesh-specs ADR 011
reads a bare tier as a Claude pin; this node narrows that to full ids
for that reason, and says so here. A Codex-pinned item with no Codex
account that has headroom — a pinned evaluation may take a paced
subscription, as `accountToFinish` does for Claude — is not evaluated:
`ErrFamilyWaits`, the item stays in evaluating for a later tick, and
`Evaluator.Waits` says it once per item (the loop's `evaluation:` error
line leaves it out). A `gpt-…` model never reaches a Claude builder:
`BuildModel` passes over it.

**The loop starts on either family's room.** `evaluateHeld` asks
`accountToFinish` for a Claude account as before; when none has room,
`Loop.CodexRoom` (`Evaluator.CodexHeadroom`) lets the evaluation start
if a Codex account has room for it. Otherwise it is counted in the
tick's one `accounts: no account has headroom` line.

**The second opinion runs on the other family.** On an urgent or high
pass: Codex after Claude (as `adr-005-codex-second-opinion`, now on the
chosen Codex account — an issued one opened for it — and unavailable
when none has headroom), Claude after Codex, on a Claude account of its
own (`Credentials.Open`, claude-code) on the item's Claude evaluate
model. The notes label each verdict by family and model —
`Codex (first evaluation, gpt-6-sol): VERDICT: PASS` and
`Claude (second opinion, opus): VERDICT …` — and the last line names
both (`(evaluated on Codex gpt-6-sol; second opinion on Claude
opus)`); the unavailable line names which family (`Claude second
opinion unavailable: …`).

**The usage report names the family.** A Codex evaluation's report
carries `adapter: codex`, the model it ran (`SessionAccount.Model`,
since Codex's stream names none), the issued credential's id and kind,
and the second opinion's tokens and cost folded in (`Carries` for a
Claude second opinion on its own account, `Add` otherwise) — still one
report on the one `EvaluateWorkItem`. No gateway contract changes.

**`mesh node start` builds the Codex harness whenever `codex` is on
PATH**, with or without a `codex:` block, since issued accounts now
supply the login. With neither a block nor an issued Codex account
there is nothing to choose and no Codex second opinion: the node
evaluates as before. The evaluation's log lines name the family and
model (`evaluate on Codex gpt-6-sol: …`, `evaluation passed on Codex
gpt-6-sol ($…)`).

## Consequences

- Normal and low work evaluates on Codex while a Codex subscription has
  room, which is what the Pro plan is bought for; Claude takes it back
  when Codex is paced, and a paced Codex still takes urgent and high.
- Urgent and high passes still get two families' judgement, whichever
  went first.
- A Codex-pinned item can wait as long as Codex has no room — visible
  once in node.log, never failed or released.
- Builders stay on Claude; a Codex builder is still to come, behind a
  per-project setting.
- The scorecard's by-family rows (runsheet) read the reports this node
  already sends.

## Amendment 2026-09-28: a Codex session is preflighted against the context server

Goal `codex-sessions-reach-the-context-tool` (item 3). On the mini the
first Codex evaluations called the built-in context tool (`prologue`)
and got an error on every call, and two of them judged blind and failed
their items: a tooling failure became a verdict. So a Codex session no
longer starts on the hope that its MCP servers work.

**The preflight.** Before any Codex session starts — the first
evaluation, once its account is open and its tools applied, and a Codex
second opinion — the evaluator calls `Evaluator.CodexPreflight`, the
Codex harness's `Preflight` (`harness.CodexPreflight`). It runs in the
`CODEX_HOME` the session will use and with exactly the environment
`Codex.Run` builds — both come from one `codexSession` (`prepareHome`
plus `SessionEnviron` with the same Unset list), so the two cannot
drift — and checks:

1. `codex mcp get prologue` succeeds: Codex parses the rendered
   `config.toml` and sees the server;
2. an MCP `initialize`, `notifications/initialized` and `tools/list`
   over streamable HTTP to the built-in's URL, with the header values
   the rendered config (`env_http_headers`) and that environment give
   Codex, answer 2xx and list at least one tool; each step within 10 s.

Its error says why: `refuses auth: <status>: <first 200 chars>` for a
401 or 403, `unreachable: <error>` for a host that does not answer,
`MESH_CODEX_MCP_<n>_TOKEN for prologue's Authorization header is
missing|empty …`, `codex mcp get prologue: …`, or `lists no tools`. It
never carries a secret (the job's and the home's are redacted). A job
with no built-in has nothing to check. The preflight costs nothing and
is not a tool call: no usage, no `recordToolUsage`.

**The fallback.** A failed preflight on an unpinned Codex evaluation
logs `item <label>: Codex preflight failed (<why>); evaluating on
Claude`, cleans up the Codex account and continues on Claude exactly as
when no issued credential has headroom: a Claude account opened, the
session `Outlive`, the same prompt. Its notes end `(evaluated on Claude
<model>; the Codex preflight failed: <why>)`, and an urgent or high
pass gets no Codex second opinion after it — `Codex second opinion
unavailable: preflight <why>`, without checking again. A Codex-pinned
item (`gpt-…`) with a failed preflight records no verdict: it is
`ErrFamilyWaits`, stays in evaluating for a later tick, and is said
once per item until a preflight passes. A Codex second opinion whose
preflight fails is `Codex second opinion unavailable: preflight <why>`
and the first verdict stands.

## Amendment, 2026-09-28: sol and luna are the defaults, not astra

Goal `codex-sessions-reach-the-context-tool` (item 5). The mini's first
live evaluations on GPT-6 Astra cost $1.29 each where the same
evaluation costs about $0.45 on GPT-6 Sol — Astra was priced for a
harder job than judging an evaluation prompt. `DefaultCodexOpus` is now `gpt-6-sol` (was `gpt-6-astra`);
`DefaultCodexSonnet` is now `gpt-6-luna` (was `gpt-6-sol`);
`DefaultCodexHaiku` stays `gpt-6-luna`. `CodexConfig.CodexModel` still
takes node.yaml's `codex: models:`
override first, so a project or operator who wants Astra sets it there;
`EvaluatorPin` still pins a full `gpt-6-astra` on an item's
`evaluateModel`, `model` or agent model, run and priced as before
(`CodexPrices` keeps Astra's rate). Nothing else in this ADR changes:
the family choice, the pin rule and the second-opinion labelling are
per-session as above, just naming Sol where the examples said Astra.
