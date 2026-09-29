---
title: Goal — The node says no verdict when its tools failed, and the two tooling failures stop happening
kind: plan
status: draft
parent: prd
summary: "`prologue: not found` is answer (b) in a narrow sense — the evaluation path was covered but its own tool-failure rule misread the context tool's `not found` answer as unreachable — and not (a): every recorded occurrence postdates the goal landing on the mini."
---

# Goal — The node says no verdict when its tools failed, and the two tooling failures stop happening

Mesh goal `evaluation-reports-no-verdict`, running. The evidence is
runsheet/handbook `research/2026-09-29-evaluations-fail-on-plumbing-not-quality.md`
(PR #52): of 59 items whose last word was `evaluation failed 3 times`,
43 carry evaluation text saying the evaluator could not run or reach
its tools. The two recurring texts are `no verdict — prologue: not
found` and `bwrap: loopback: Failed RTM_NEWADDR`.

## prologue: not found — which is true

**(b)** is the answer, and only in a narrow sense: `codex-sessions-reach-the-context-tool`
did not cover the evaluation path correctly — its own rule on that
path was wrong in one case, not that the path went unbuilt or
unwatched — **and not (a)**: every recorded occurrence of `prologue:
not found` postdates that goal landing on the mini.

The goal did run on the evaluation path: #286 preflights a Codex
evaluation against the built-in context server, #289 approves its
calls (the `default_tools_approval_mode = "approve"` root cause), and
#305 adds `Evaluator.toolBroken`, a rule that runs on exactly that
path. But #305's rule read the context tool's own `not found` answer —
a `mesh_document` slug it does not have — as proof the tool was never
reached, and discarded a verdict a session had actually earned. That
is a false classification by the goal's own rule on a path it covered,
not an unreachable tool and not an uncovered path — which is why (b)
only holds narrowed to that one case.

### The items

Read through `mesh_items` on 2026-09-29 at about 13:02Z (project
`runsheet/mesh` for `a-finished-session-leaves-no-process`,
`builtin-is-mesh-context` and `signed-releases-on-runsheet-mesh`;
project `runsheet/runsheet` for the rest). These are the items whose
`last_error` is `evaluation failed 3 times` and whose evaluation notes
carry `prologue: not found`. Every such note opens `Codex (first
attempt, gpt-6-sol): no verdict — prologue: not found`. Times are UTC,
from each evaluation's `at`, and **P** marks the evaluations carrying
that text.

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
| runsheet/runsheet | slipway-first-build/8 | 08:57:39, 09:21:49 **P**, 09:29:02 |

That is 13 items. Across every item with any such evaluation, not only
the ones that failed three times, the earliest is 2026-09-29
01:14:55Z (`a-finished-session-leaves-no-process/17`, confirmed by the
same `mesh_items` read). The last read carrying the text is 11:39:30Z
(`attested-validation-replaces-duplicate-ci/32`). None falls before
09-29 01:14Z. The evaluations that failed three times before then
(`signed-releases-on-runsheet-mesh/4`, `/6` and `/11`,
`ci-fits-the-mini/5`) are ordinary review findings, not this text.
Most of the 13 also carry `RTM_NEWADDR` on another evaluation, the
goal's other failure and independent of this one.

### Against the merges and the mini's version

Merge times, `gh pr view <n> --repo runsheet/mesh-cli --json mergedAt`:

| What | Merged (main) |
|---|---|
| #258 prompts name `mesh_document`/`mesh_search` on mesh-context | 2026-09-28 07:28:56Z |
| #285 a Codex MCP call's error text is kept | 2026-09-28 10:57:35Z |
| #286 a Codex evaluation is preflighted | 2026-09-28 11:00:40Z |
| #289 the root cause: `default_tools_approval_mode = "approve"` | 2026-09-28 11:17:09Z |
| #305 a tool failure is never a verdict (`toolBroken`) | 2026-09-28 22:19:35Z |
| #264 the served built-in is mesh-context | 2026-09-29 01:19:46Z |
| #396 the same, re-landed with the prologue alias | 2026-09-29 06:52:27Z |

What the mini ran, per `specs/goal-codex-sessions-reach-the-context-tool.md`'s
own statement and `docs/releases-after-transfer.md`:

- **From 2026-09-28 07:18Z it ran 9ed5f8b.** This predates every one
  of the above, #258 included: #258 merged at 07:28:56Z, eleven minutes
  after the mini started running 9ed5f8b, so that binary does not carry
  it either. Chris parked the Codex credential (`limitCredentialNodes`
  to the retired laptop node) at 07:35Z, so no Codex evaluation ran on
  the mini again until it was un-parked.
- **From 2026-09-29 00:51:59Z it has run v0.2.188** (commit 5a8c29e,
  confirmed by node.log: `2026/09/29 00:51:59 mesh node v0.2.188` and
  `confirming update 4d8d435 → v0.2.188: start 1 of 3`). That version
  contains #258, #285, #286, #289 and #305, but not #264 or #396 —
  both merged after 00:51:59Z, so the running binary predates them.
  `mesh version` on the mini still answers `v0.2.188`, and it cannot
  follow main until `RELEASE_TAG_TOKEN` is set.

The text `no verdict — <tool>: <error>` exists only from #305 onward,
so every recorded occurrence was written by v0.2.188 — after the goal
landed on the mini. That binary preflights the built-in, approves its
calls and judges the evaluation path (#286, #289, #305), so the path
was covered; the earliest occurrence, 01:14:55Z, is after the mini
started running it at 00:51:59Z. This rules out (a) — the failures do
not predate the goal — and confirms the path was exercised, which is
why (b) only holds in the narrow sense above, not as the brief words
it plainly.

### The code path that said `not found`

The mini keeps the Codex sessions it runs under
`~/.mesh/work/kept-sessions/<session>/output.jsonl` (24 h, one
directory per session — the same convention every kept Codex or Claude
stream uses). Read directly on the mini on 2026-09-29 at about 12:10Z:
of that window's kept sessions, 291 have an `mcp_tool_call` naming
server `prologue`, the built-in's rendered name on this pre-#396
binary. A reader can recount them the same way: `grep -l
'"server":"prologue"' ~/.mesh/work/kept-sessions/*/output.jsonl | wc
-l` for the session count, then `grep '"type":"mcp_tool_call"'` on
those files for the calls themselves, split by `"status"` and
`"error"`. Across those 291 streams:

- 1,096 calls ended `completed`.
- 382 calls ended `failed` with `"error": null` and the server's own
  result, `{"content":[{"type":"text","text":"not found"}]}`.
- 5 calls ended `failed` with a real `error` (never reached the
  server).

The 382 are `mesh_document` calls for slugs that do not exist. Each is
a context file's *name* with `.md` dropped, not the slug from
INDEX.md's `slug` column: the top ones are
`instructions-runsheet-runsheet-agents` (55),
`instructions-runsheet-mesh-cli-agents` (35),
`adr-adr-003-enterprises` (25), `adr-adr-013-node-releases-on-merge`
(21) and `adr-adr-032-attested-validation-replaces-duplicate-ci` (20).
The file `adr-adr-032-….md` is slug `adr-032-…`.

The chain, in code as it stood before this goal's fix:

1. `ParseCodexStream` (`internal/harness/codexstream.go`) counts a
   `failed` `mcp_tool_call` as an error. `codexCallError` has no
   `error` field to read on these calls, so it falls back to the
   result's text, and `FirstError` becomes `not found`. `ParseStream`
   (`internal/harness/stream.go`) does the same for a Claude
   `is_error` tool_result.
2. `Evaluator.toolBroken` (`internal/node/evaluate.go`) called a
   server broken whenever `Errors >= Calls` on it. A session whose
   only context calls asked for wrong slugs therefore looked exactly
   like one that never reached the tool, and yielded `prologue: not
   found` as its evaluation text.
3. `afterEvaluate` discarded that verdict, wrote `Codex (first
   attempt, gpt-6-sol): no verdict — prologue: not found` at the head
   of the item's notes, and re-ran the job on Claude — whose verdict,
   often itself blocked by `RTM_NEWADDR`, became the recorded fail.

### What closes it

Item 6 of this goal (already on this branch, PR #433) changes step 2
of that chain. `harness.ToolStat` now splits a server's errored calls
into `Clean` (answered without an error) and `Answered` (answered with
the tool's own error result — MCP `isError`, e.g. this `not found`):
the call reached the server either way. `ToolStat.Reached()`
(`internal/harness/harness.go`) is `Clean+Answered > 0`.
`Evaluator.toolBroken` now calls a server broken only when it has
errors and was never reached (`s.Errors > 0 && !s.Reached()`,
`internal/node/evaluate.go`) — a call refused before the server
(approval, auth, transport, not connected) still counts as broken, but
a `not found` answer no longer does, so the verdict it belongs to is
kept. The context rule every prompt carries also now names where the
slug comes from: `internal/harness/prompt.go`'s context line reads
"the document's slug — INDEX.md's `slug` column, never a context file's
name: a file name is not a slug, `adr-adr-001-….md` is slug
`adr-001-…`", closing the mismatch between what a session guessed and
what `mesh_document` actually takes.

## What landed

_(item 5 fills this in.)_

## The mini's upgrade

_(item 5 fills this in.)_

## Tainted evaluations before and after

_(item 5 fills this in.)_

## The goal's other pieces

The no-verdict outcome reported to the platform and
`bwrap: loopback: Failed RTM_NEWADDR` are this goal's other items.

## Log

- 2026-09-29 · item 7: re-verified the item dates through `mesh_items`,
  the merges through `gh pr view`, and the kept-session counts
  directly on the mini; restated the answer as (b) in the narrow
  sense, not (a), and closed the doc with item 6's `Reached`/`Clean`/
  `Answered` fix and the prompt's slug sentence.
- 2026-09-29 · item 7, retry: restored the brief's kept-stream counts
  (1,096 completed, 382 answered `not found`, 5 real errors, over 291
  sessions) with a `grep`-able file pattern under
  `~/.mesh/work/kept-sessions/`, and fixed the mini-version sentence —
  9ed5f8b at 07:18Z predates #258 too (merged 07:28:56Z), not every
  merge "except #258" as the last attempt had it.
