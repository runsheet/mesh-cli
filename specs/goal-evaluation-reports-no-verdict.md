---
title: Goal — The node says no verdict when its tools failed, and the two tooling failures stop happening
kind: plan
status: draft
parent: prd
summary: "`prologue: not found` postdates codex-sessions-reach-the-context-tool (all of it on the mini's v0.2.188, 2026-09-29) and is (b): that goal's tool-failure rule on the evaluation path read the context tool's own `not found` — a slug it does not have — as an unreachable tool; the node now counts an answered error as reached."
---

# Goal — The node says no verdict when its tools failed, and the two tooling failures stop happening

Mesh goal `evaluation-reports-no-verdict`, running. The evidence is
runsheet/handbook `research/2026-09-29-evaluations-fail-on-plumbing-not-quality.md`
(PR #52): of 59 items whose last word was `evaluation failed 3 times`,
43 carry evaluation text saying the evaluator could not run or reach its
tools. The two recurring texts are `no verdict — prologue: not found`
and `bwrap: loopback: Failed RTM_NEWADDR`.

## prologue: not found — which is true

**(b): `codex-sessions-reach-the-context-tool` did not cover this
failure on the evaluation path.** The failures do not predate it.
Every one of them was produced by a binary that already carried that
goal's work, and the context tool was reachable in every one. What
produced `not found` was the goal's own tool-failure rule (#305,
`Evaluator.toolBroken`). It treated the context tool's *answer*, `not
found` for a document slug it does not have, as a tool the session
could not reach. It then discarded the verdict. The rename window
(prompt and config naming the built-in differently) is ruled out as the
cause; see below.

### The items

Read through `mesh_items` on 2026-09-29 at about 12:10Z. These are the
items whose `last_error` is `evaluation failed 3 times` and whose
evaluation notes carry `prologue: not found`. Every such note opens
`Codex (first attempt, gpt-6-sol): no verdict — prologue: not found`.
Times are UTC, from each evaluation's `at`, and **P** marks the
evaluations with that text.

| Project | Item | Evaluations (2026-09-29 unless noted) |
|---|---|---|
| runsheet/mesh | a-finished-session-leaves-no-process/24 | 09:48:45, 10:20:15 **P**, 10:32:27 **P** |
| runsheet/mesh | builtin-is-mesh-context/1 | 09-28 22:28:08, 06:22:16 **P** |
| runsheet/mesh | signed-releases-on-runsheet-mesh/18 | 01:40:25 **P**, 01:46:28, 02:19:39 |
| runsheet/runsheet | a-node-has-an-owner/1 | 02:42:31, 04:29:45, 05:06:18 **P** |
| runsheet/runsheet | a-node-has-an-owner/8 | 08:23:11, 09:22:50 **P**, 10:41:25 **P** |
| runsheet/runsheet | attested-validation-replaces-duplicate-ci/4 | 01:37:40, 02:11:18 **P**, 03:23:29 **P** |
| runsheet/runsheet | attested-validation-replaces-duplicate-ci/26 | 08:28:33, 10:25:04 **P**, 10:34:22 |
| runsheet/runsheet | credentials-follow-the-enterprise/2 | 02:51:30 **P**, 04:54:35, 05:49:05 |
| runsheet/runsheet | credentials-follow-the-enterprise/3 | 01:40:32, 03:27:22, 04:16:00 **P** |
| runsheet/runsheet | enterprise-creation-makes-its-tenant/3 | 09-28 13:27:47, 01:36:14 **P**, 03:11:09 **P** |
| runsheet/runsheet | enterprise-creation-makes-its-tenant/8 | 09:39:16 **P**, 10:10:26, 10:17:13 |
| runsheet/runsheet | slipway-first-build/7 | 08:07:31 **P**, 08:33:47, 08:42:58 |
| runsheet/runsheet | slipway-first-build/8 | 08:57:39 n, 09:21:49 **P**, 09:29:02 |

That is 13 items. Across every item with any such evaluation, not only
the ones that failed three times, the first is 2026-09-29 01:14:55Z
(a-finished-session-leaves-no-process/17). The last read is 11:39:30Z
(attested-validation-replaces-duplicate-ci/32). None falls before 09-29
01:14Z. The evaluations that failed three times before then
(signed-releases-on-runsheet-mesh/4, /6 and /11, ci-fits-the-mini/5)
are ordinary review findings. Most of these items also carry
`RTM_NEWADDR` on another evaluation, which is the goal's other failure
and independent of this one.

node.log on the mini agrees. It holds 83 lines reading `evaluation on
Codex gpt-6-sol saw no context (prologue: not found); not a verdict,
evaluating on Claude opus`, plus the same text as `Codex second opinion
unavailable`. The first is at 2026-09-29 01:13:03Z and they are still
occurring at 11:51Z.

### Against the merges and the mini's version

| What | Merged (main) |
|---|---|
| #258 prompts name `mesh_document`/`mesh_search` on mesh-context | 2026-09-28 07:28:56Z |
| #285 a Codex MCP call's error text is kept | 2026-09-28 10:57:35Z |
| #286 a Codex evaluation is preflighted | 2026-09-28 11:00:39Z |
| #289 the root cause: `default_tools_approval_mode = "approve"` | 2026-09-28 11:17:09Z |
| #305 a tool failure is never a verdict (`toolBroken`) | 2026-09-28 22:19:35Z |
| #264 the served built-in is mesh-context | 2026-09-29 01:19:45Z |
| #396 the same, re-landed with the prologue alias | 2026-09-29 06:52:27Z |

What the mini ran:

- **From 2026-09-28 07:18Z it ran 9ed5f8b.** This predates all of the
  above except #258
  (`specs/goal-codex-sessions-reach-the-context-tool.md`). The Codex
  credential was parked at 07:35Z, and node.log has no Codex
  evaluation from then until 09-29.
- **From 2026-09-29 00:51:59Z it has run v0.2.188** (commit 5a8c29e,
  `docs/releases-after-transfer.md`). That version contains #258, #285,
  #286, #289 and #305, but not #264 or #396. `mesh version` on the mini
  still answers `v0.2.188`, and it cannot follow main until
  RELEASE_TAG_TOKEN is set (`specs/goal-node-releases-on-merge.md`).
  Its first Codex evaluation is at 00:59:47Z.

The text `no verdict — <tool>: <error>` exists only from #305 onward,
so every recorded occurrence was made by v0.2.188. That binary
preflights the built-in, approves its calls, and treats a tool failure
as no verdict. The failures postdate the goal, which rules out (a).

### The code path that said `not found`

The mini keeps the Codex sessions it runs under `~/.mesh/work/kept-sessions/`
(24 h). Their streams show what happened. The built-in, rendered
`[mcp_servers.prologue]` by v0.2.188, started, listed its tools and
answered:

- In 291 kept sessions that called it, 1,096 calls ended `completed`.
- 382 calls ended `failed` with `"error": null` and the server's own
  result, `{"content":[{"type":"text","text":"not found"}]}`.
- Only 5 calls ended with an `error`, meaning they never reached the
  server.

The 382 are `mesh_document` calls for slugs that do not exist. Each is
a context file's *name* with `.md` dropped, not the slug from INDEX.md's
`slug` column: `instructions-runsheet-runsheet-agents` (55),
`instructions-runsheet-mesh-cli-agents` (35),
`adr-adr-003-enterprises` (25), `adr-adr-013-node-releases-on-merge`
(21) and `adr-adr-032-attested-validation-replaces-…` (20). The file
`adr-adr-032-….md` is slug `adr-032-…`.

The chain, in code:

1. `ParseCodexStream` (`internal/harness/codexstream.go`) counts a
   `failed` `mcp_tool_call` as an error. `codexCallError` has no
   `error` to read, so it falls back to the result's text, and
   FirstError becomes `not found`. `mcpStartupServers` is not
   involved: no startup error was streamed. `ParseStream`
   (`stream.go`) does the same with a Claude `is_error` tool_result.
2. `Evaluator.toolBroken` (`internal/node/evaluate.go`) calls a server
   broken when `Errors >= Calls`. A session whose only context calls
   asked for wrong slugs therefore looked like one that never reached
   the tool, and yielded `prologue: not found`.
3. `afterEvaluate` discarded that verdict. It wrote `Codex (first
   attempt, gpt-6-sol): no verdict — prologue: not found` at the head
   of the notes and re-ran the job on Claude. Claude's verdict (often
   blocked by `RTM_NEWADDR`) became the recorded fail.

**The rename window is ruled out.** v0.2.188 prompts for
`mesh_document` (#258) and serves it under `prologue`. Codex calls
tools by name within whatever server lists them, so the calls reached
`prologue` and 1,096 completed. The mismatch never stopped a call. The
server name in the note is just the name v0.2.188 renders.

### Fixed on this branch

- **`harness.ToolStat.Answered` counts the errored calls the tool
  answered itself.** In Codex that is a `failed` call with no `error`
  and a result (`codexToolAnswered`). In Claude it is an `is_error`
  tool_result from a server the init event named `connected`.
  `ToolStat.Reached()` is true when any call was answered, cleanly or
  with the tool's own error. `toolBroken` now asks `Reached()`, so a
  context tool that said `not found` leaves the verdict standing.
  Calls refused before the server (approval, auth, not connected)
  still make a session tool-broken.
  - Tests: `TestParseCodexStreamCountsTheToolsOwnErrorAnswered` (on
    the mini's stream shape, `testdata/codex_mcp_not_found.jsonl`),
    `TestParseStreamCountsTheToolsOwnErrorAnswered` and
    `TestAContextToolThatAnsweredIsAVerdict`.
- **The context rule every prompt carries now names where the slug
  comes from:** INDEX.md's `slug` column, never a file name here, with
  the example `adr-adr-001-….md` → `adr-001-…`. Test:
  `TestEvaluatePromptContextTools`.
- **An evaluation session on each family gets a context tool it can
  reach.** The job is built as `Evaluator.Evaluate` builds it
  (`SessionTools.Apply` with `MeshContextMCP`).
  - **Codex** (`TestEvaluationOnCodexReachesTheContextTool`): the test
    asserts the exact `config.toml` text. It holds
    `[mcp_servers.mesh-context]`, the gateway's `/mcp` url,
    `env_http_headers = { "Authorization" = "MESH_CODEX_MCP_0_TOKEN" }`
    and `default_tools_approval_mode = "approve"`. The session's env
    carries `MESH_CODEX_MCP_0_TOKEN=Bearer <token>`, and
    `Codex.Preflight` passes against an httptest MCP fake that was sent
    exactly that header.
  - **Claude** (`TestEvaluationOnClaudeReachesTheContextTool`): the
    test asserts the exact `--mcp-config` JSON,
    `--allowedTools mcp__mesh-context__*`, and the sandbox's
    file-path form. A tools/list POST to the rendered url with the
    rendered header is answered.
  - `TestCodexEvaluationPreflightsMeshContext`: the job the evaluator
    hands its preflight carries `mesh-context`.

### After merge

The mini needs this binary: a release once RELEASE_TAG_TOKEN is set,
or the by-hand `mesh node upgrade --binary`. Then
`saw no context (…: not found)` should stop appearing in node.log. A
Codex evaluation's usage row may still show mesh-context errors, which
are now the tool's answers, and its verdict is recorded.

## The goal's other pieces

The no-verdict outcome reported to the platform and
`bwrap: loopback: Failed RTM_NEWADDR` are this goal's other items. The
before-and-after classification (tainted evaluations as a share of
failed ones, starting at 90%) is owed once the change is on the mini.

## Log

- 2026-09-29 · item 2: `prologue: not found` traced to toolBroken
  reading the tool's own `not found` as unreachable (answer (b));
  fixed, with the both-families context tests.
