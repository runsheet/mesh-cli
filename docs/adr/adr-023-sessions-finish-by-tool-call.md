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
   When the hosted tool has already moved the item on the platform, the
   node's own finish is refused for the item's state: a refusal
   (`gateway.IsRefusal`, not `IsUnsupported`) after a `mesh_finish`
   call is logged `item <label>: finished by tool call on the platform`
   and the build is handed in, not failed.
3. **The evaluator ends by `mesh_verdict`, the planner by
   `mesh_plan`** (item 5 of the goal): the evaluator's verdict and
   failed clauses, and the planner's graph, are read from those calls'
   inputs first; `VERDICT:` and the fenced JSON stay as the parsed
   fallback, logged once per session the same way. Until item 5 lands
   they are parsed from the text alone.

## Consequences

- A session that ran out of turns after its finish call, or whose
  closing prose broke the format, still hands its work in.
- The final-message contracts in `internal/harness/prompt.go` are kept,
  unchanged: an older platform without the hosted tools, a Codex
  session whose MCP failed, or a model that forgets the call ends as
  before, and the fallback log line says how often that happens.
- The node still pushes and still reads back what it pushed; the tool
  call says *that* the session is done and how, never which commit.
- `WorkCalls` holds only the work tools' inputs — they carry ids,
  branches, evidence and verdict text, never a secret — and evidence is
  redacted like the final message before it is handed in.
