---
summary: Build, evaluate and plan sessions end by calling the hosted mesh-work tools — mesh_finish, mesh_verdict, mesh_plan (mesh_fail to give up) — whose inputs the harness keeps on Result.WorkCalls; the node hands in what the call says, and the final-message parse (ALREADY DONE:, VERDICT:, the fenced JSON) stays as a fallback, logged once per session.
status: accepted
---

# ADR-023: Sessions finish by tool call

2026-10-03, goal `every-actor-has-a-name` (item 4: the build; item 5 the
evaluator and the planner). Numbered 023 beside
`adr-023-the-planner-splits-a-broad-goal.md` and
`adr-023-mesh-mcp-claude-code-is-a-personal-node.md`, which landed the
same day; cite this one as "ADR-023, finish by tool call".

## Context

A session's end is read from its final message: the builder's
`ALREADY DONE:` line (adr-003), the evaluator's `VERDICT: PASS|FAIL`
and `FAILED: … || SATISFY: …` lines, the planner's fenced JSON graph.
A model that forgets the format, wraps it in prose, or runs out of turns
before writing it leaves the node guessing — a built item with an
unparseable message is still pushed, an evaluation with no verdict
line is a no-verdict, a plan with a broken fence is re-asked. The
platform's runsheet goal `mesh-work-tools-on-the-hosted-mcp-server`
adds the mesh-work tools to the hosted `/mcp` — the same endpoint
`MeshContextMCP` already configures as `mesh-context` on the node's
token, with `--allowedTools mcp__mesh-context__*` — so a session can say
how it ended as a structured call rather than as text. The runsheet
TRD's slice 6 (`trd-6a-slice-6-mesh-control-plane`) names no second
server, so the tools ride `mesh-context`; if the platform ever serves
them as their own server, it goes beside `mesh-context` as `mesh-work`
on the same token.

## Decision

1. **The harness keeps the work tools' inputs.** `tools.IsWorkTool`
   names them: `mesh_finish`, `mesh_fail`, `mesh_verdict`, `mesh_plan`,
   `mesh_progress`. `ParseStream` keeps a Claude `tool_use` block's
   `input`, and `ParseCodexStream` an `mcp_tool_call`'s `arguments`, on
   `Result.WorkCalls` — the tool's own name, without its
   `mcp__<server>__` prefix, to the last call's input. Other tools'
   inputs are never kept.
2. **The build ends by `mesh_finish`.** `BuildPrompt`'s
   `# Finish by tool call` tells the session to call
   `mesh_finish(item_id, branch, head_sha)` when it built the item, or
   `mesh_finish(item_id, outcome="already_done", evidence)` when main
   already holds it, and `mesh_fail(item_id, error)` when it cannot be
   done; the `ALREADY DONE:` line stays in the prompt as the fallback
   contract. In `Runner.afterBuild`, after a successful session:
   - a `mesh_finish` call is the finish, whatever the final message
     says or whether there is one: `outcome: already_done` with
     evidence hands in `FinishWorkItemAlreadyDone` with no push, else
     the node validates and pushes its own checkout as before and hands
     in `FinishWorkItem` on the head *it* pushed — the session's
     `branch` and `head_sha` are not trusted over the push;
   - a `mesh_fail` call (with no `mesh_finish`) fails the item with its
     `error`;
   - with neither, the text is parsed as before (`ParseAlreadyDone`)
     and one line, `item <label>: no mesh_finish call; parsed the final
     message`, is logged for the session.
   **Amended 2026-10-10** (goal `a-build-pushes-before-it-finishes`,
   items 4 and 5): the build's finish is recorded by the node after the
   push, and by the node alone. A node build session finishes on the
   node's own stdio MCP server, `mesh-node` (`harness.NodeWorkMCP`:
   `mesh session-tools` on the host, a Node script beside `mcp.json` in
   a pod), whose `mesh_finish` and `mesh_fail` answer at once and record
   nothing; the hosted `mcp__mesh-context__mesh_finish` and
   `mcp__mesh-context__mesh_fail` are `--disallowedTools` for a build
   (`ClaudeCode.Args`, `SandboxArgs`). The call still lands on
   `Result.WorkCalls` by the tool's own name, and `Runner.afterBuild`
   takes it in this order: validate, `Workspacer.Push`, and only once
   the push has returned the sha it pushed, `FinishWorkItem` on that
   sha (or, with no push, `FinishWorkItemAlreadyDone`). A push that
   fails is the build's failure — `FailWorkItem` with `push
   <branch>: <err>`, no finish of either kind sent, the branch left for
   a retry: a finish whose push failed is not a finish. With nothing
   recorded by the session's call, a refusal of the node's finish is
   the finish's error; the old reading of it as the hosted tool's
   finish (`finishedByTool`, `item <label>: finished by tool call on the
   platform`) is retired. Before this the hosted call reported the
   finish first, the evaluation of the item claimed its checkout and
   rewrote the credential helper with a read token, and the build's
   push was refused (`Write access to repository not granted`).
   Evaluation and plan sessions keep the hosted `mesh_verdict` and
   `mesh_plan` (Decisions 3 and 4).
3. **The evaluator ends by `mesh_verdict`** (item 5 of the goal).
   `EvaluatePrompt`'s `# Finish by tool call` tells the session to call
   `mesh_verdict(item_id, passed, notes, failed_clauses: [{clause,
   satisfy}], readings: [{clause, reading}])` and to write its review
   and its `VERDICT: PASS|FAIL`, `FAILED: … || SATISFY: …` and
   `READING: … || …` lines in the final message as well — the fallback
   contract, unchanged. `verdictOf` reads a session's verdict, in
   `Evaluator.afterEvaluate` for the first evaluation and in
   `secondOpinion` for a blind attempt's retry and the second opinion,
   on Claude and Codex alike:
   - a `mesh_verdict` call with a readable `passed` (a bool, or
     `"true"`/`"false"`) is the verdict, accepted with an empty final
     message (`harness.VerdictCall`): its `notes`, each `readings` entry
     appended to them as a `READING:` line — so `logReadings`, the notes'
     truncation that keeps readings, and `NoteSameClause` on a repeated
     clause run exactly as on the text path — and its `failed_clauses`,
     both halves required, duplicates dropped, at most `ClauseMax`
     (none given: the notes' `FAILED:` lines are read), all redacted
     like the text;
   - with no call the final message is parsed as before
     (`ParseVerdict`, `ParseFailedClauses`) and one line, `item <label>:
     no mesh_verdict call; parsed the final message` (a second session's
     with its role, `item <label>: second opinion: …`), is logged per
     session;
   - with neither, it is no verdict (`noVerdict`, runsheet ADR 035), as
     before.
   A refusal of the node's `EvaluateWorkItem` after the deciding
   session's `mesh_verdict` call is the hosted tool having recorded it:
   logged `item <label>: evaluated by tool call on the platform`, not an
   error. A refusal with no call recorded (`verdictCalled` false) and
   `not yours` in its text (ADR 009's "you are not the node evaluating
   the item") is the same shape of race lost the other way: another
   node's own verdict is the one of record, and retrying this session's
   forever would never land it — logged once `item <label>: verdict
   refused: another node holds the evaluation`, the item forgotten
   (`LeftRunning.Forget`) rather than held for a next no-verdict note
   (mesh-cli goal `an-items-evaluation-runs-on-one-node`, item 2). Any
   other refusal is still `record verdict: <err>`, an error retried on
   the next tick.
4. **The planner ends by `mesh_plan`** (item 5). `PlanPrompt`'s
   `# Finish by tool call` tells the session to call `mesh_plan(goal_id,
   items, questions)`, each item the object the JSON block carries
   (seq, title, brief, acceptance, depends_on, repo, base_ref, model,
   model_reason, atomic, precondition), `checkpoints` or `split` beside
   them when rule 3 asks, and to write the fenced block in the final
   message as well — the fallback contract, unchanged. In
   `Planner.readPlan` a `mesh_plan` call with `items` or `split` is
   rendered as the fenced block its message would end with
   (`harness.PlanCall`; an array sent as a JSON string decoded first)
   and read by `ParsePlan` and `ParsePlanQuestions` into the same
   `PlannedSpec`s, then through the same checks — the repo map, the model
   rubric (`PlannableModel`), `explainedDeps`, `hostReadingAcceptance`,
   `closeOutAcceptance`, `sameAcceptanceAsFailed`, `reLands`,
   `notASplit`, `otherRepoItems` — so a plan by call and a plan by text
   cannot differ in what is handed in. With no call the text is parsed
   and one line, `plan <goal>: no mesh_plan call; parsed the final
   message`, is logged per session. A `planGoal` refused after a call is
   still the plan's error, as before.

## Consequences

- A session that ran out of turns after its finish call, or whose
  closing prose broke the format, still hands its work in.
- The final-message contracts in `internal/harness/prompt.go` are kept,
  unchanged and load-bearing — `VERDICT: PASS|FAIL`, the `FAILED:` and
  `READING:` lines, the planner's fenced JSON, `ALREADY DONE:`: an older platform without the hosted tools, a Codex
  session whose MCP failed, or a model that forgets the call ends as
  before, and the fallback log line says how often that happens.
- The node still pushes and still reads back what it pushed; the tool
  call says *that* the session is done and how, never which commit.
- `WorkCalls` holds only the work tools' inputs — they carry ids,
  branches, evidence and verdict text, never a secret — and evidence is
  redacted like the final message before it is handed in.
