---
summary: Before an evaluation starts the node chooses its family per session — Codex when a Codex account (issued or node.yaml's) has headroom for the goal's priority, else Claude; a full gpt-… id pins Codex and waits in evaluating without headroom, a claude-… id pins Claude; the urgent/high second opinion runs on the other family, labelled by family and model; the one usage report names adapter codex and the gpt-6 model of a Codex evaluation; before any Codex session starts the node preflights the built-in context server in the session's own home and env (`codex mcp get` and a tools/list handshake), and a failure moves an unpinned evaluation to Claude, keeps a pinned one waiting and makes a Codex second opinion unavailable. Amended: an unpinned Codex evaluation defaults to gpt-6-sol (opus tier) or gpt-6-luna (sonnet, haiku), astra only when pinned; and a tool failure is never a verdict — a session that saw no context is evaluated again on the other family, or the item waits in evaluating. Amended 2026-09-29: an evaluation that reached no verdict — no VERDICT line, or no context and no other family — is recorded with evaluateWorkItem's outcome no_verdict and its reason, never a fail (a gateway without the field: it waits in evaluating, said once per run); a family blind on the first attempt is not asked for the second opinion; and the preflight probes Codex's sandbox (`codex sandbox -c sandbox_mode="workspace-write" -- true`, the session's own policy), and a Codex session whose every command bubblewrap refused (`Result.SandboxBroken`) is never a verdict either — every `bwrap:` text in the record came from a Codex session. Amended 2026-09-29 again: a Codex evaluation's sandbox gets the network (`sandbox_workspace_write.network_access=true` on `codex exec` and on the probe), so its tests can bind loopback; the probe binds a socket (`mesh sandbox-probe`) instead of running `true`; and a session whose test commands that executed every one died on `socket: operation not permitted` (or `bind:`) is `SandboxBroken` too. The mini's `codex-bwrap` AppArmor profile makes bwrap start; it does not give the sandbox loopback.
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

1. `codex mcp get mesh-context` succeeds: Codex parses the rendered
   `config.toml` and sees the server;
2. an MCP `initialize`, `notifications/initialized` and `tools/list`
   over streamable HTTP to the built-in's URL, with the header values
   the rendered config (`env_http_headers`) and that environment give
   Codex, answer 2xx and list at least one tool; each step within 10 s.

Its error says why: `refuses auth: <status>: <first 200 chars>` for a
401 or 403, `unreachable: <error>` for a host that does not answer,
`MESH_CODEX_MCP_<n>_TOKEN for mesh-context's Authorization header is
missing|empty …`, `codex mcp get mesh-context: …`, or `lists no tools`. It
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

## Amendment 2026-09-28: a tool failure is never a verdict

Goal `codex-sessions-reach-the-context-tool` (item 6). Two Codex
evaluations on the mini judged blind — every call to the built-in
context server failed — and their FAIL verdicts sent their items back
to `ready`, a rework each.

- **A session that saw no context gives no verdict.** Before a verdict
  is read, the evaluator judges the session's tools from `Result.Tools`
  (`Evaluator.toolBroken`): it is *tool-broken* when a built-in server
  of its MCP map (the context server, `mesh-context`) or a required console
  tool it was served has calls and every one errored, or has an error
  and no call at all (not connected at startup). One clean call is
  enough: 3 calls and 1 error is a verdict as before. A tool-broken
  session's verdict is discarded, whatever it said.
- **It is evaluated on the other family.** node.log says `item <label>:
  evaluation on <family model> saw no context (<tool>: <first error, 200
  chars>); not a verdict, evaluating on <other family model>`, and the
  same job — the same prompt and workspace — runs on the other family
  as the evaluation proper, not a second opinion: Claude on an account
  of its own after Codex, Codex with headroom for the goal's priority
  after Claude. Its verdict is the verdict. The notes open with the
  attempt that gave none — `<Family> (first attempt, <model>): no
  verdict — <tool>: <error>` — then the verdict's notes and `(evaluated
  on <the family that gave it>)`; the one usage report folds both
  sessions' cost, tokens and per-tool rows. The urgent/high second
  opinion still applies to a pass, on the family that gave no verdict.
- **Or it waits.** When the other family cannot run — the item pins its
  family, the node has no Codex harness or account, no Codex account
  has headroom, the session was adopted after a restart (its prompt is
  not kept), or the other session errors, reaches no verdict or is
  tool-broken too — nothing is recorded: `Evaluate` returns
  `ErrNoContext` and the item stays in `evaluating` for a later tick.
  node.log says why once per item (`Evaluator.Blind`, as `Waits`):
  `…saw no context (…); not a verdict, and <Family> cannot evaluate it:
  <why>; it stays in evaluating for a later tick (said once for the
  item)`. `Blind` is one per node, Codex binary or not, shared by the
  evaluator every tick builds afresh and the adopted ones; the loop's
  own `evaluation:` error line leaves `ErrNoContext` to it, as it does
  `ErrFamilyWaits`. The item never goes back to `ready` on a tool error.
- A second opinion that is tool-broken is unavailable (`saw no context
  (<tool>: <error>)`), and the first verdict stands.

## Amendment 2026-09-29: no verdict is an outcome, and a blind family is not asked again

Goal `evaluation-reports-no-verdict` (item 1). The record (runsheet/handbook
`research/2026-09-29-evaluations-fail-on-plumbing-not-quality.md`) has 59
items whose last word was `evaluation failed 3 times`; on 43 the evaluator
could not run or reach its tools, and 10 more carry a `VERDICT: PASS`.

- **The outcome.** An evaluation the evaluator had no way to judge is
  recorded as no verdict, never as a fail:
  `Gateway.EvaluateWorkItemNoVerdict(ctx, itemID, reason, costCents,
  usage)` sends `evaluateWorkItem` with `outcome: "no_verdict"`, the
  reason in `notes`, the sessions' cost in `costCents` and the one usage
  report in `usage` — and no `passed`. The platform half (runsheet goal
  `evaluation-reports-no-verdict-platform`, its item 1) had not landed
  when this was written, so the field `outcome` and the value
  `no_verdict` are the node's assumption, named once in
  `gateway.EvaluationOutcomeField` and `gateway.EvaluationNoVerdict`;
  if the platform lands another name, those two constants change.
- **The three cases** (`Evaluator.noVerdict`):
  (a) the deciding session's text has no `VERDICT:` line
  (`harness.ParseVerdict` not ok) — reason `no VERDICT line: <the first
  300 chars of its text>`;
  (b) the session saw no context (`toolBroken`) and the other family ran
  but could not evaluate it either — reason `evaluation on <family
  model> saw no context (<tool>: <error>); not a verdict, and the
  evaluation on <other> is unavailable too: <why>`;
  (c) the session saw no context — its context server, a required
  console tool or its sandbox unavailable, as `toolBroken` reports it —
  and there is no other family to run it on — reason `…; not a verdict,
  and <Family> cannot evaluate it: <why>`.
  node.log says `item <label>: evaluation reached no verdict on <family
  model>, recorded as no verdict ($<cost>): <reason>`. A session that
  does return a verdict records pass or fail exactly as before, and a
  verdict the other family reached after a blind first attempt keeps its
  `<Family> (first attempt, <model>): no verdict — …` head.
- **An older gateway.** Whether `EvaluateWorkItemInput` has `outcome` is
  asked once by introspection and kept for `ScalingProbeEvery`, as
  `holding` is (ADR-004: never a field the platform does not accept).
  Without it the call returns `gateway.ErrNoVerdictOutcome`, nothing is
  sent, and the node keeps what it did before: the item waits in
  `evaluating` for a later tick (`ErrNoContext` for (b) and (c), the
  `evaluator session reached no verdict` error for (a)), and node.log
  says `the platform takes no no-verdict outcome; a no-verdict
  evaluation waits in evaluating (said once per run)` once per run
  (`Evaluator.NoVerdict`, one `OncePer` per node).
- **The pass in the record was the node's.** Read through `mesh_items`
  (runsheet/mesh): `the-node-serves-its-owners-orgs/1` (attempt 2),
  `a-finished-session-leaves-no-process/24` (attempts 2 and 3) and
  `signed-releases-on-runsheet-mesh/18` (attempt 1) each read `Codex
  (first attempt, gpt-6-sol): no verdict — prologue: not found`, then
  `Claude (first evaluation, opus): VERDICT: PASS`, then `Codex (second
  opinion, gpt-6-sol): VERDICT: FAIL`. The goals were high priority, so
  the pass took the second opinion — and by this ADR's earlier amendment
  ("the urgent/high second opinion still applies to a pass, on the
  family that gave no verdict") it ran on Codex, the family that had
  just failed to reach its context; this time it made no failing call
  (so `toolBroken` did not catch it) and failed the item on what it
  could not read. `passed = passed && op.passed` then sent
  `passed:false`: the node's fold, not the platform's. Now a family that
  was blind on the first attempt of an evaluation is not asked for its
  second opinion: the verdict the other family reached stands, and the
  notes say `<Family> second opinion unavailable: it saw no context on
  the first attempt (<tool>: <error>)`. That sentence of the earlier
  amendment is replaced by this one.

## Amendment 2026-09-29: a sandbox that will not start is never a verdict

Goal `evaluation-reports-no-verdict` (item 3). Codex runs on the host,
not in the node's pod (mesh-specs ADR 007), under `codex exec --sandbox
workspace-write`, whose sandbox is Codex's bundled bubblewrap. On the
mini (Ubuntu, `kernel.apparmor_restrict_unprivileged_userns = 1`) it
fails to set up its network namespace — `bwrap: loopback: Failed
RTM_NEWADDR: Operation not permitted` — so every command the session
runs is refused before it starts, the acceptance's `go test` or `npm
test` never runs, and the turn still completes with a verdict the
evaluator had no way to reach.

**The record.** Of every evaluation in project runsheet/mesh read on
2026-09-29 (`mesh_items`, all 52 goals), 34 carry `bwrap` in their
notes, on 19 items of 6 goals, all on 2026-09-29. **Every one of those
texts came from a Codex session (gpt-6-sol); none from Claude.** 28
were Codex evaluations alone; in the other 6 Claude opus judged too
(5 as the first evaluation with a Codex second opinion, 1 as the
second opinion after Codex), and in each the bwrap text sits inside
the Codex section of the notes. That is as expected: a Claude session
runs in the node's Podman pod or plainly on the host and never uses
bwrap. The record moves — items are replaced and their evaluations
with them, and a later read counted 33 — but the finding does not: no
`bwrap` text in it came from a Claude session.

- **The probe joins the preflight.** `Codex.Preflight`, after the
  context-server check (and for a job with no built-in too), runs
  Codex's sandbox on a no-op under the session's own policy — `codex
  sandbox -c sandbox_mode="workspace-write" -- true` (`harness.SandboxProbe`;
  codex-cli 0.157 has no `sandbox linux` or `debug landlock`
  subcommand, and `codex sandbox` alone defaults to read-only, so the
  `sandbox_mode` override makes it the `workspace-write` that `codex
  exec` runs) — in the session's workspace, its own
  `CODEX_HOME` and the environment `Run` builds, within
  `PreflightTimeout`. A `bwrap:` line on stderr or a non-zero exit fails
  it: `sandbox: <first 200 characters>`. The preflight's semantics hold
  unchanged: an unpinned evaluation moves to Claude (`Codex preflight
  failed (sandbox: …); evaluating on Claude`, and in the notes), a
  pinned one waits in evaluating said once for the item, a Codex second
  opinion is `unavailable: preflight sandbox: …`.
- **The stream says it too.** `ParseCodexStream` marks a completed
  `command_execution` whose status is `failed` and whose aggregated
  output carries `bwrap:`; when every command the session ran was
  refused that way, `Result.SandboxBroken` is the first bwrap line
  (capped at 300). One clean command leaves it empty.
- **A sandbox-blocked session is never a verdict.** The evaluator treats
  `SandboxBroken` exactly as a tool failure (`Evaluator.unreadable`,
  ahead of `toolBroken`): its text is never read by `ParseVerdict`,
  whatever `VERDICT:` it wrote; node.log says `evaluation on <family
  model> ran in a sandbox that blocked every command (sandbox: <line>);
  not a verdict`; the same job runs on the other family, the notes
  opening `Codex (first attempt, <model>): no verdict — sandbox:
  <line>`; with no other family it is recorded as no
  verdict (case (c) of the amendment above; on a gateway without the
  outcome nothing is recorded, `ErrNoContext`, and the item stays in
  evaluating, said once). A Codex second opinion
  with it is unavailable: `sandbox broken (<line>)`, and the first
  verdict stands.
- **The operator's fix** is in the README (`Codex on a node`, The
  sandbox on Ubuntu): an AppArmor profile granting `userns` to the
  `codex` binary on a root-owned path, or the host-wide sysctl, each
  with what it costs in isolation. Until one is in place the probe
  keeps Codex evaluations off the host.

## Amendment 2026-09-29: a tool that answered was reached

Goal `evaluation-reports-no-verdict` (item 6, retrying item 2's code
half). On the mini, `no verdict — prologue: not found` discarded
verdicts from sessions whose context tool worked. The kept Codex
streams show the built-in answering 1,096 calls `completed` and 382
`failed` with its own result `{"content":[{"type":"text","text":"not
found"}]}` and `error` null: a `mesh_document` slug it does not have —
a context file's name with `.md` dropped (`adr-adr-003-enterprises`)
instead of INDEX.md's `slug` column. Every such answer counted as an
error, and `toolBroken`'s rule, every call errored, called the tool
unreachable. A tool that answered was reached.

- **`Clean` and `Answered` are counted, never derived.**
  `harness.ToolStat.Clean` counts the calls the server answered without
  an error (a Claude tool_result not `is_error`, a Codex `mcp_tool_call`
  `completed`); `ToolStat.Answered` counts those it answered with an
  error result of its own — in Codex a `failed` call whose `error` is
  null or absent and whose `result` is present and not null, judged on
  the JSON values and never their text (an `error` with an empty
  message is still Codex's refusal; a result with only structured
  content is still the tool's answer), in Claude an `is_error` tool_result
  from a server the init event named `connected` whose text is not
  Claude Code's own refusal of the call (`claudeClientErrors`:
  `<tool_use_error>`, `No such tool available`, permission refusals, the
  MCP client's `-32000`/`-32001`, connection closed, request timed out,
  not connected). A call refused before the server — a Codex call with
  an `error` such as `MCP tool call requires approval, but approval
  policy is never` — is neither. Neither is `Calls − Errors`: a server
  not connected at startup adds an Error with no Call, so a startup
  error followed by one clean call (Calls 1, Errors 1) would read as
  unreached. `Calls`, `Errors`, `FirstError`, `Wall` and the usage
  report's `tools` rows are unchanged.
- **`ToolStat.Reached()` is `Clean + Answered > 0`**, and `toolBroken`
  names a server broken only when `Errors > 0 && !Reached()`: every
  call refused before the server, or a startup error and no call
  reached. The adopted-session `prologue` alias (until 2026-10-28) and the
  required-console-tool branch stand. The built-in's `not found` is the
  tool working: the verdict it came with stands, on the family that
  reached it, and nothing runs on the other family.
- **The slug.** The prompts' context rule says the `mesh_document` slug
  is INDEX.md's `slug` column, never a context file's name
  (`adr-adr-001-….md` is slug `adr-001-…`), so the `not found` answers
  themselves become rarer.
- **Reachability is proved on both families by test**: the Claude
  evaluation job's `--mcp-config` carries `mesh-context` at the
  gateway's `/mcp` with `Authorization: Bearer <token>`; the Codex job's
  `CodexMCPConfig` carries `[mcp_servers.mesh-context]` at that URL with
  the header from its variable; the Codex preflight receives the very
  job the evaluator builds; and `Codex.Preflight` passes against a fake
  MCP server answering initialize and tools/list on that config, and
  names the reason (`refuses auth: 401`) when it refuses.

## Amendment 2026-09-29: an evaluation's sandbox has the network, and the probe binds a socket

Goal `evaluation-reports-no-verdict` (item 9). Since 12:59Z on
2026-09-29 the mini has `/etc/apparmor.d/codex-bwrap`, a profile with
`userns,` on the bubblewrap Codex ships
(`~/.local/share/codex/codex-resources/bwrap`, `flags=(unconfined)`;
README, The sandbox on Ubuntu). bwrap starts: kept streams from 13:13Z
show `go vet` and `go build` exiting 0. But `go test ./...` dies with
`listen tcp6 [::1]:0: socket: operation not permitted` — the
workspace-write sandbox has the network off by default, and Codex's
seccomp filter then refuses `socket()` altogether, loopback included.
Every `httptest` test, every `npm test` against a local server and
every `gh` is unrunnable, the evaluator wrote verdicts with the tests
unrun, and the probe above passed, because `true` opens no socket.
`codex-bwrap` makes bwrap start; it does not give the sandbox loopback.

- **Network on for the sandboxed commands.** `Codex.Args` carries
  `-c sandbox_workspace_write.network_access=true`
  (`harness.SandboxNetwork`, codex-cli 0.157's key for the
  workspace-write table, default false) after `--sandbox
  workspace-write`. Every Codex session today is an evaluation (a
  first evaluation or a second opinion); builders on Codex come later
  and inherit it. On the mini, `codex sandbox -c
  sandbox_mode="workspace-write" -- mesh sandbox-probe` fails with
  `listen tcp 127.0.0.1:0: socket: operation not permitted`, and the same
  with the key set prints a 127.0.0.1 address and exits 0. **The price**:
  a command the evaluator runs may reach the network — the LAN, the
  internet — not just loopback; Codex 0.157 has no loopback-only
  setting. The sandbox still confines writes to the workspace, the
  session still holds only its item's token and its own `CODEX_HOME`,
  and an evaluation whose tests cannot run is worth nothing. Runnable
  tests are the point of the evaluation; that is the price paid for
  them.
- **The probe binds a socket.** `harness.SandboxProbe(probe)` is
  `codex sandbox -c sandbox_mode="workspace-write" -c
  sandbox_workspace_write.network_access=true -- <probe>`, the same
  keys as the session (`SandboxNetwork` is shared, so the check cannot
  drift from the run), and the probe is this executable's hidden `mesh
  sandbox-probe` (`os.Executable()`, `harness.ProbeLoopback`): listen on
  127.0.0.1:0, print the address, exit 0; else exit 1 with the listen's
  error. A failure names the bwrap line, else the socket line
  (`sandbox: Error: listen tcp 127.0.0.1:0: socket: operation not
  permitted`), else the output — with the preflight's semantics
  unchanged (an unpinned evaluation to Claude, a pinned one waits, a
  second opinion unavailable).
- **The socket refusal is a second `SandboxBroken` cause.** For a node
  upgraded before a host's config is fixed — or a Codex whose key
  means something else — `ParseCodexStream` sets `Result.SandboxBroken`
  to the first line matching `socket: operation not permitted` or
  `bind: operation not permitted` when every test command that
  executed failed with it — or, with none executed, when every failed
  command did. A test command is one of whose simple commands, past the
  shell wrapper, assignments and `env`/`timeout`, starts with a runner
  (`go test`, `npm test`, `npm run test`, `pnpm`/`yarn test`, `npx
  vitest|jest`, `pytest`, `cargo test`, `make test|check`); a runner's
  name in quoted text — `rg 'go test'`, an echo — is no test. It
  executed when it carries an exit code other than 126 or 127 (`go:
  command not found`) and Codex did not decline it; one that did not
  counts neither way, so a search or a declined test beside a refused
  one never turns unrunnable tests into a verdict. One test command that passed, or that
  failed on its own merits with no refusal in its output, leaves it
  empty: that test ran, and the verdict stands. The evaluator handles it as
  the bwrap cause: never read for a verdict, the other family, else
  recorded as no verdict with `sandbox: <the socket line>`.

## Amendment 2026-10-02: wording corrected to mesh-context

Goal `builtin-is-mesh-context`. The preflight section above named the
built-in `prologue` after the rename had already reached the node's
own attachment (`adr-006`'s 2026-09-28 amendment): `codex mcp get
prologue`, the error texts naming `prologue`'s Authorization header,
and the `toolBroken` paragraph's "the context server, `prologue`". All
now read `mesh-context`: the preflight runs `codex mcp get
mesh-context`, its errors name `mesh-context`'s header and command, and
`toolBroken` names `mesh-context` as the context server. Nothing about
what the preflight checks or how `toolBroken` judges a session
changes — only the name. The dated narrative above (the first Codex
evaluations' own `prologue: not found` results, read on 2026-09-29 and
before) stays as it was recorded: it quotes what those sessions said,
not what the node does now.
