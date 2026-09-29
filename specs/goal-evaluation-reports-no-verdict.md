---
title: Goal — The node says no verdict when its tools failed, and the two tooling failures stop happening
kind: plan
status: draft
parent: prd
summary: "`prologue: not found` is (b) in the narrow sense — codex-sessions-reach-the-context-tool covered the evaluation path, but its tool-failure rule read the context tool's own `not found` answer as an unreachable tool — and not (a): the mini ran v0.2.188 from 2026-09-29 00:51:59Z and the earliest occurrence is 01:14:55Z."
---

# Goal — The node says no verdict when its tools failed, and the two tooling failures stop happening

Mesh goal `evaluation-reports-no-verdict`, running. The evidence is
runsheet/handbook `research/2026-09-29-evaluations-fail-on-plumbing-not-quality.md`
(PR #52). The two recurring texts are `no verdict — prologue: not
found` and `bwrap: loopback: Failed RTM_NEWADDR`; this document
answers which of the goal's two readings is true for the first.

## prologue: not found — which is true

It is **(b) in the narrow sense and not (a)**: every occurrence
postdates the mini running v0.2.188, which carries
`codex-sessions-reach-the-context-tool`, from 2026-09-29 00:51:59Z
(the earliest occurrence is 01:14:55Z), and that goal did cover the
evaluation path — #286 preflights a Codex evaluation, #289 approves its
MCP calls, #305 adds `Evaluator.toolBroken` — but #305's rule read the
context tool's own `not found` answer to a wrong `mesh_document` slug
as a tool that was never reached, and discarded the verdict.

So (b) holds only narrowed to that one case: the path was neither
unbuilt nor unwatched, and the tool was reachable; the goal's own rule
on that path misclassified the tool answering.

### The items

Read through `mesh_items` on 2026-09-29 at 14:17Z (project
`runsheet/mesh` for `a-finished-session-leaves-no-process`,
`builtin-is-mesh-context` and `signed-releases-on-runsheet-mesh`;
project `runsheet/runsheet` for the rest). These are the items whose
`last_error` is `evaluation failed 3 times` and whose evaluation notes
carry `prologue: not found`; every such note opens `Codex (first
attempt, gpt-6-sol): no verdict — prologue: not found`. Times are UTC,
from each evaluation's `at`; **P** marks the evaluations carrying that
text.

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

`builtin-is-mesh-context/1` lists only its second and third
evaluations; the first is not on the record. Beyond these 13, the
earliest evaluation carrying the text on any item is 2026-09-29
01:14:55Z (`a-finished-session-leaves-no-process/17`, its only
evaluation, the item failed for another reason), and the latest the
same read found is 11:39:30Z (`attested-validation-replaces-duplicate-ci/32`,
still `ready`). None falls before 09-29 01:14Z.

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

What the mini ran, from `~/.mesh/node.log`'s start lines
(`grep -a ' mesh node v0\.\| mesh node [0-9a-f]\{7\},' ~/.mesh/node.log`)
and `specs/goal-codex-sessions-reach-the-context-tool.md`:

- **From 2026-09-28 07:18Z, 9ed5f8b.** That predates every merge above,
  #258 included (merged eleven minutes later, 07:28:56Z).
- **From 2026-09-28 11:36:23Z, 4d8d435** (node.log: `mesh node
  4d8d435`). It predates #305, so it could not write `no verdict —
  <tool>: <error>`.
- **From 2026-09-29 00:51:59Z, v0.2.188** (commit 5a8c29e; node.log:
  `2026/09/29 00:51:59 mesh node v0.2.188` and `confirming update
  4d8d435 → v0.2.188: start 1 of 3`, restarted on the same version at
  04:00:32Z). It contains #258, #285, #286, #289 and #305, not #264 or
  #396, so it still renders the built-in as server `prologue`.
- From 2026-09-29 14:13:19Z the mini is moving to v0.2.191; that is
  after every count in this section's evidence and is item 12's
  subject.

The text `no verdict — <tool>: <error>` exists only from #305 onward,
and the first binary on the mini carrying #305 is v0.2.188, started
00:51:59Z; the earliest occurrence is 01:14:55Z. The failures therefore
do not predate the goal — not (a) — and were written by the binary
that preflights, approves and judges the evaluation path.

### The code path that said `not found`

The mini keeps each Codex session's stream 24 hours under
`~/.mesh/work/kept-sessions/<session>/output.jsonl`, so the count
below is dated: counted on the mini on 2026-09-29 at 14:16Z, over the
files last written at or before the cutoff **2026-09-29 14:00:00Z**
(the oldest kept stream then was last written 01:01:52Z), every one
written by v0.2.188.

```sh
cd ~/.mesh/work/kept-sessions
FILES=$(find . -path './*/output.jsonl' ! -newermt '2026-09-29 14:00:00Z' \
  | xargs grep -l '"server":"prologue"')
echo "$FILES" | wc -l                                   # sessions
Q='select(.type=="item.completed" and .item.type=="mcp_tool_call" and .item.server=="prologue")'
cat $FILES | jq -c "$Q | select(.item.status==\"completed\")" | wc -l
cat $FILES | jq -r "$Q | select(.item.status==\"failed\" and .item.error==null)
  | [(.item.result.content[0].text // \"<none>\" | .[0:70]), .item.tool] | @tsv" | sort | uniq -c
cat $FILES | jq -r "$Q | select(.item.status==\"failed\" and .item.error!=null)
  | .item.tool" | sort | uniq -c
```

| Kept streams to 2026-09-29 14:00:00Z | Count |
|---|---|
| Sessions with a call to server `prologue` | 320 |
| Calls `completed` | 1,330 |
| Calls `failed` with `error` null (the server answered with its own error result) | 425 |
| — `not found` | 403 |
| —— `mesh_document` | 383 |
| —— `mesh_items` | 17 |
| —— `mesh_documents` | 3 |
| — any other text | 22 |
| —— `mesh_health_service answers only the estate (a token with ns.runsheet)` | 8 |
| —— `a project is named org/slug` (4 `mesh_document`, 2 `mesh_documents`) | 6 |
| —— `validating "arguments": … required: missing properties` (2 `mesh_document`, 2 `mesh_items`) | 4 |
| —— `validating "arguments": … unexpected additional properties` (1 each `mesh_search`, `mesh_items`, `mesh_health_nodes`, `mesh_evidence`) | 4 |
| Calls `failed` with a real `error` (`tool call failed for prologue/…`; 3 `mesh_health_stuck`, 1 `mesh_goals`, 1 `mesh_documents`) | 5 |

No call ended in any other status. So the tool answered: of the 425
null-error failures, 403 are its `not found`, 383 of those to
`mesh_document`, and only 5 calls never reached it. The `mesh_document`
misses asked, for the most part, for a context file's *name* with
`.md` dropped (`instructions-runsheet-runsheet-agents`,
`adr-adr-003-enterprises`) rather than the slug in INDEX.md's `slug`
column — the file `adr-adr-032-….md` is slug `adr-032-…`.

The chain, in code as it stood before this goal's fix:

1. `ParseCodexStream` (`internal/harness/codexstream.go`) counts a
   `failed` `mcp_tool_call` as an error. These calls carry no `error`,
   so `codexCallError` falls back to the result's text and the
   server's `FirstError` becomes `not found`. `ParseStream`
   (`internal/harness/stream.go`) does the same for a Claude
   `is_error` tool_result.
2. `Evaluator.toolBroken` (`internal/node/evaluate.go`) called a server
   broken whenever `Errors >= Calls` on it, so a session whose only
   context calls asked for wrong slugs looked exactly like one that
   never reached the tool.
3. `afterEvaluate` discarded that verdict, wrote `Codex (first attempt,
   gpt-6-sol): no verdict — prologue: not found` at the head of the
   item's notes, and re-ran the job on Claude — whose verdict, often
   itself blocked by `RTM_NEWADDR`, became the recorded fail.

### What closes it

Items 6 and 8 of this goal (on this branch's base,
`mesh/evaluation-reports-no-verdict-8`) change step 2. `harness.ToolStat`
(`internal/harness/harness.go`) counts, beside `Calls` and `Errors`:

- `Clean` — the calls the server answered **without** an error (a
  Claude tool_result not `is_error`, a Codex `mcp_tool_call`
  `completed`), counted as they happen, never `Calls` less `Errors`;
- `Answered` — the calls the server itself answered **with** an error
  result (MCP `isError`, e.g. `not found` for a slug it does not
  have); each is one of `Errors` too. A call refused before the server
  (approval, permission, transport, not connected) is neither.

`Reached()` is the one line

```go
return s.Clean+s.Answered > 0
```

and `Evaluator.toolBroken` now calls a server broken only when
`s.Errors > 0 && !s.Reached()`. A `not found` answer is `Answered`, so
the server was reached and the verdict is kept; a call refused before
the server still counts as broken. The context line every prompt
carries (`internal/harness/prompt.go`) now names where the slug comes
from: "the document's slug — INDEX.md's `slug` column, never a context
file's name: a file name is not a slug, `adr-adr-001-….md` is slug
`adr-001-…`".

## What landed

_(item 12 fills this in.)_

## The mini's upgrade

_(item 12 fills this in.)_

## Tainted evaluations before and after

_(item 12 fills this in.)_

## Log

- 2026-09-29 · item 7: first answer — (b) in the narrow sense, not
  (a) — with the items through `mesh_items`, the merges through `gh
  pr view` and a kept-stream count; three evaluations failed it on
  consistency (a count against no stated cutoff, the #258 chronology,
  totals that did not add up, `Clean` described as errored calls).
- 2026-09-29 · item 11: rewrote from item 7's last revision.
  Re-verified the 13 items through `mesh_items` at 14:17Z and the
  merges through `gh pr view`; added 4d8d435 from node.log to the
  mini's versions; recounted the kept streams once at the 14:00:00Z
  cutoff into one table (320 sessions, 1,330 completed, 425 answered
  with an error — 403 `not found`, 22 other — and 5 real errors); and
  described `Clean`, `Answered` and `Reached()` as `harness.go` has
  them.
- 2026-09-29 · item 11, second attempt: the answer, the tables and the
  counts are unchanged. Item 8's work has since landed on main as one
  squashed merge, so the branch merged `origin/main` and `git diff
  origin/main --stat` names this document alone; the `Reached()` line
  quoted above is re-checked against `harness.go` on main.
