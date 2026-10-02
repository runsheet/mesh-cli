---
title: "Goal — Context is fetched, not shipped: a session's disk holds only its repos' instructions and its own goal, everything else comes through the MCP tools, and median input per build falls under 1M tokens"
kind: plan
status: draft
parent: prd
summary: "Before (mesh node usage, 2026-09-30, records 00:01:13..07:25:17Z): 137 builds, median input 1,282,352 tokens, median context 309,796 bytes; after: owed — the window opened at the mini's first start of v0.2.218 or later (v0.2.218, the first release carrying #560, 2026-10-02T06:24:47Z), and its first full day, 2026-10-03, has not passed; the first three context lines read 16,825–26,969 bytes; verdict owed."
---

# Goal — Context is fetched, not shipped: a session's disk holds only its repos' instructions and its own goal, everything else comes through the MCP tools, and median input per build falls under 1M tokens

Mesh goal `context-is-fetched-not-shipped` (project `runsheet/mesh`),
running; set 2026-10-01T07:25:22Z. It supersedes
`context-budget-that-holds`.

## Statement

Chris, 2026-10-01: "I thought our MCP server was meant to avoid the
filesystem stuff?" It was: mesh-specs `trd` §3, as of 2026-09-21, put
instructions and ADRs on disk as binding, the PRD and TRD within a
budget, an INDEX.md listing every document plus the live goals (ADR
003), and the MCP server "so it fetches the rest on demand". In
practice the on-disk set swallowed the on-demand one.

Measured on the mini on 2026-09-30 (node v0.2.209, with
context-budget-that-holds items 1–4, 9 and 10 on board): a build's
`.mesh/context/` was 309KB median (430KB for `a-node-has-an-owner`)
against the 240KB budget; INDEX.md alone was 190KB — 116KB of two
document tables with 1,003 rows and about 70KB of a live section
listing every live goal's items — and nine ADRs rode along at 120KB
whether the item touched them or not. Every prompt said to read
INDEX.md first (`contextRule`), so the first turns pulled about 75K
tokens into the transcript and re-read it on every turn after: builds
averaged 81–99K input per turn, the big ones 90+ turns at 130–150K per
turn, 12–14M tokens each; 124 builds spent 355M input tokens that day,
97% cache reads, and both Claude subscriptions' seven-day windows went
from under 50% to over 90% in 36 hours. The MCP tools were "served,
never called" in most sessions; one evaluator fetched one 17KB
document all day.

Invert the default:

1. The context directory holds only what the session cannot do
   without — the item's own repos' instructions (AGENTS.md/CLAUDE.md as
   mesh indexes them) and the session's own goal with its items
   (statement, acceptance, item table) — target under 40KB, hard cap
   80KB, logged per session as today.
2. No INDEX.md table of every document and no other goals' items: a
   short INDEX.md (a screen, not a table) names the project, says what
   is on disk, and says how to find the rest — `mesh_search`,
   `mesh_document`, `mesh_documents`/`mesh_projects`, `mesh_items` —
   with the ADRs and the TRD named as binding and searched before any
   design decision.
3. The prompts (build, evaluate, plan, rebase, research) say "search
   before you read, read a document only when the work touches it",
   and the plan prompt tells the planner to name in each item's brief
   the documents the builder must fetch (slugs), so a build fetches two
   or three documents, not forty.
4. `mesh_search` and `mesh_document` return bounded bodies (a summary
   and the sections asked for, or a byte cap with "fetch section X for
   more"), so a fetch is never a 60KB paste.
5. The usage record and `mesh node usage` keep `contextBytes` and add a
   count of documents fetched by tool, so the reading shows the shift.
6. ADR-015/016 and mesh-specs `trd` §3 and `adr-003` are rewritten for
   the inverted default; this goal supersedes context-budget-that-holds
   (cancelled 2026-10-01 with items 17–19 unlanded; its merged items
   1–4, 7, 9, 10 and 13 stay, and this document records them).

The acceptance is the number: the reading before (`mesh node usage`
on the mini's node.log for 2026-09-30: median input per build session,
median context bytes) and after, on the same projects over a day, both
in this document. If median input per build does not fall below 1M
tokens (from ~2.9M) the change is wrong — say so here rather than
argue the mechanism.

## Acceptance

`go vet ./...`, `gofmt -l .` empty, `go build ./cmd/mesh/` and the
linux/amd64 cross-build, `go test ./...` green. Tests: a build's
context directory holds only its repos' instructions and its own goal
(an item of a project with 1,000 documents and 12 live goals writes
under 40KB and never more than 80KB); INDEX.md has no document table
and no other goal's items; the prompts carry the search-first rule and
the planner's fetch list; `mesh_search`/`mesh_document` bodies are
bounded; the usage record carries the documents-fetched count. Live,
on the mini after the upgrade: per-session context lines read under
80KB; `mesh node usage` for one full day shows median input per build
session under 1M tokens and median context bytes under 80KB, with the
2026-09-30 reading beside it in this document; ADR-015/016, `trd` §3
and `adr-003` amended. CI green.

## Items

From `mesh_items` (project `runsheet/mesh`, goal
`context-is-fetched-not-shipped`) read 2026-10-02 at about 06:25Z, the
merges from `gh pr view`, and the models from node.log's `build on
<model>` and `evaluate on <family> <model>` lines for each item. Every
evaluation of this goal ran on Codex gpt-6-sol; none had a second
opinion. The mesh-specs pull requests are as the item record links
them (`gh` could not read `runsheet/mesh-specs` from this checkout).

| # | Title | Repo | Outcome | PR | Built on |
|---|---|---|---|---|---|
| 1 | Node: a session's context holds only its repos' instructions and its own goal — no ADR, PRD or TRD bodies, a one-screen INDEX.md, 40KB target and 80KB cap; ADR-015 rewritten | mesh-cli | failed — evaluation failed 3 times (04:13, 04:22, 04:29Z); its branch's work was carried and finished as item 6 | — | opus (three builds); evaluated on gpt-6-sol |
| 2 | Harness: every prompt says search before you read, and the plan prompt names the documents the builder must fetch | mesh-cli | merged 2026-10-02T04:26:32Z (523e0d6, first in v0.2.212) | [#544](https://github.com/runsheet/mesh-cli/pull/544) | sonnet; passed on gpt-6-sol first time |
| 3 | Node: the usage record and `mesh node usage` carry the documents a session fetched by tool; ADR-016 amended | mesh-cli | merged 2026-10-02T05:03:37Z (add7ff4, first in v0.2.214) | [#548](https://github.com/runsheet/mesh-cli/pull/548) | sonnet (two builds); failed once, then passed on gpt-6-sol |
| 4 | mesh-specs: TRD §3's context paragraphs and ADR 003 record the inverted default | mesh-specs | merged | [mesh-specs#126](https://github.com/runsheet/mesh-specs/pull/126) | sonnet twice, then opus (sharpened after the second failure); passed on gpt-6-sol |
| 5 | Goal doc: context-is-fetched-not-shipped (first version) | mesh-cli | cancelled — pending, never started, replaced by item 9 and then this item | — | none (no session ran) |
| 6 | Node: a session's context holds only its own repos' instructions (keyed by repo) and its own goal; 80KB cap enforced on the bytes written, stale context always cleared; ADR-015 rewritten | mesh-cli | merged 2026-10-02T06:00:01Z (9219031, first in **v0.2.218**) | [#560](https://github.com/runsheet/mesh-cli/pull/560) | opus (two builds, rebased on opus); failed once, then passed on gpt-6-sol |
| 7 | Node: the documents-fetched count, gateway fallback tested, older-platform refusals treated as landed; ADR-016 amended | mesh-cli | merged 2026-10-02T05:18:37Z (6d8db42, first in v0.2.215) | [#555](https://github.com/runsheet/mesh-cli/pull/555) | opus (rebased on opus); passed on gpt-6-sol first time |
| 8 | mesh-specs: TRD §3 and ADR 003 record the inverted default (expect already_done if #126 merged) | mesh-specs | merged | [mesh-specs#129](https://github.com/runsheet/mesh-specs/pull/129) | sonnet; passed on gpt-6-sol first time |
| 9 | Goal doc: context-is-fetched-not-shipped | mesh-cli | failed — its first attempt (sonnet) failed evaluation at 06:10:38Z on a `title:` that did not parse as YAML; its second (sonnet, 06:12:13Z) failed on `session credentials: Post "https://api.runsheet.dev/graphql": context deadline exceeded`, nothing written | — | sonnet; evaluated on gpt-6-sol |
| 10 | Goal doc: context-is-fetched-not-shipped — what landed, what context-budget-that-holds left, the before and after readings, the verdict | mesh-cli | running — this document | — | opus |

## What context-budget-that-holds left

Goal `context-budget-that-holds` set a total budget on the old default
(node.yaml `context:`, 240KB/60KB) and made the node report what its
context cost. It was cancelled on 2026-10-01 with items 17–19
unlanded — item 17 (INDEX.md's live section inside `bytes_max`, every
other goal one row), item 18 (its goal doc) and item 19 (the mesh-specs
paragraph for 17). This goal supersedes it; its merged items stay on
main, and this goal builds on them:

| Item | What stays | PR | Merged |
|---|---|---|---|
| 1 | node.yaml `context: {bytes_max, doc_bytes_max}`, re-read every tick (`ContextConfig`, `Loop.ContextBudget`) — the knobs ADR-015 now sets to 80KB | [mesh-cli#421](https://github.com/runsheet/mesh-cli/pull/421) | 2026-09-29 12:04Z |
| 2 | instructions and ADRs counted against the total, the item's own repos first; ADR-015's first version (kept as its history) | [mesh-cli#423](https://github.com/runsheet/mesh-cli/pull/423) | 2026-09-29 12:27Z |
| 3 | INDEX.md summaries only for kinds fetched and not on disk (superseded by the one-screen index) | [mesh-cli#422](https://github.com/runsheet/mesh-cli/pull/422) | 2026-09-29 13:26Z |
| 4 | the per-session line `<label>: context <n> bytes in <k> files, <m> documents to fetch` and `contextBytes` on the usage record; ADR-016 | [mesh-cli#429](https://github.com/runsheet/mesh-cli/pull/429) | 2026-09-29 12:59Z |
| 7 | mesh-specs: TRD §3 and adr-003 name the context budget and `contextBytes` | [mesh-specs#101](https://github.com/runsheet/mesh-specs/pull/101) | merged (date not read) |
| 9 | every session's usage record logged on its label with its kind, and `mesh node usage` reading node.log (item 6's work on main) | [mesh-cli#464](https://github.com/runsheet/mesh-cli/pull/464) | 2026-09-29 16:24Z |
| 10 | INDEX.md inside the budget, reserved first; ADR-015's 2026-09-29 amendment | [mesh-cli#463](https://github.com/runsheet/mesh-cli/pull/463) | 2026-09-29 15:48Z |
| 13 | mesh-specs: TRD §3 says INDEX.md is inside `bytes_max`; adr-003 decision 3 amended | [mesh-specs#107](https://github.com/runsheet/mesh-specs/pull/107) | merged (date not read) |

Its pull requests #448 (item 6), #466 (item 11, RFC 3339 `--from`/
`--to` and `--project`) and #472 (item 14) were closed unmerged, so
`mesh node usage` still takes whole days only. Items 5, 8, 12, 15 and
16, its goal docs, failed; it never recorded an after reading. The
mechanics of items 1–4 and 10 are kept, as decided, under ADR-015's
"History: the 2026-09-29 version" and ADR-016's first decision.

## What the node does now

**The context directory** (ADR-015 as rewritten by #560;
`internal/node/workspace.go`). `FetchContext` puts on disk only the
instructions of the session's own repos (`ContextOptions.Repos`), each
keyed by repo and path as `instructions-<owner>-<repo>-<path>.md`
(`contextFileName`) and read from the session's own checkout when the
slug is one several repos share, and the session's own goal
(`ContextOptions.Goal`: statement, acceptance, items; none for
research). No ADR, PRD or TRD body is fetched. `RenderIndex` writes a
one-screen INDEX.md with no document table and no other goal: what is
on disk, the `mesh_search`/`mesh_document`/`mesh_documents`/
`mesh_projects`/`mesh_items` tools for the rest, the ADRs and TRD
named binding, one count line, then `## Goal`. node.yaml's `bytes_max`
defaults to 80KB with INDEX.md included; `RenderIndex` never goes past
the room the files leave (the item table cut at a row, then the
statement and acceptance), and `WriteContext` clears `.mesh/context`
first, measures what it rendered and writes nothing past the cap
(`Context.fit`). Past the 40KB target (`ContextBytesTarget`) a session
logs `ContextWritten.TargetWarning`.

**The prompts** (#544; `internal/harness/prompt.go`). `contextRule`,
carried by every prompt (build, evaluate, plan, rebase, research), says
INDEX.md names what is on disk — just the session's repos'
instructions and its own goal — and to search before reading: find
with `mesh_search`, read with `mesh_document` only when the work
touches it, other projects through `mesh_projects`/`mesh_documents`,
live state through `mesh_items`, the ADRs and TRD searched before any
design decision, "a build fetches two or three documents, not forty".
`PlanPrompt` tells the planner that each item's brief names, by slug,
the two or three documents the builder must fetch, found with
`mesh_search` while it plans.

**The usage record** (ADR-016's 2026-10-02 amendments, #548 and #555;
`internal/node/usagereading.go`). Every session's record keeps
`contextBytes` and adds `documentsFetched` — its `mesh_document` and
`mesh_search` calls on the built-in server, the `prologue` alias
folded in — and `toolNames`, its MCP calls per `<server>__<tool>`; a
platform without either field loses that field alone. `ReadUsage` and
`UsageReading.Lines` print, per kind, sessions, the input-tokens median
and mean, the cache-read share, the context-bytes median and how many
records carry it, and the new `documents fetched median` and `fetched
by tool` columns (`-` and `0` over records older than v0.2.214, which
carry neither).

## Left to the platform

These are runsheet/runsheet's, not this repo's:

- **Bounded bodies for `mesh_search` and `mesh_document`** (point 4).
  The tools are the platform's `/mcp` server; the node renders the
  attachment and counts the calls, it does not shape what a call
  returns. `trd` §3 Tools, "Bounded bodies", records it: `mesh_search`
  returns an excerpt around the match, `mesh_document` a summary and
  the sections asked for or, with none named, the body up to a byte cap
  with "fetch section X for more" — **owed to runsheet/runsheet, goal
  `context-is-fetched-not-shipped`, as of 2026-10-02**. It is not there
  yet: this item's own `mesh_document` read of `trd` returned the whole
  117KB body.
- **The usage fields.** `trd` §3, "The context budget" and the usage
  record's paragraphs: `contextBytes` rides `usage: UsageReport` and
  is **owed to runsheet/runsheet** ("`UsageReportInput` does not yet
  take `contextBytes`; until it does, the node sends it once, the
  platform refuses the unknown field"), and `documentsFetched` rides
  beside it on the same fallback. `trd` §3 counts `mesh_search`,
  `mesh_document`, `mesh_documents`, `mesh_projects` and `mesh_items`
  calls; the node's `documentsFetched` (ADR-016) counts `mesh_document`
  and `mesh_search` and carries every tool's calls in `toolNames`, so
  the platform can read either. `adr-003-plan-from-the-truth`'s
  2026-10-02 amendment narrows decision 3's live listing to the
  session's own goal and points to `trd` §3 for the contract; it adds
  no field of its own.

## The mini

`grep -a ' mesh node v0\.' ~/.mesh/node.log ~/.mesh/node.log.1`, read
2026-10-02T06:23Z (start lines, UTC; node.log.1 runs from
2026-09-30T00:00:05Z to 2026-10-01T16:33Z, node.log from there):

- 2026-09-30 00:00:05 v0.2.208 (confirming v0.2.191 → v0.2.208), then
  v0.2.208 and v0.2.209 alternating until 02:41:17 v0.2.209;
- 2026-09-30 06:51:11, 2026-10-01 04:26:36 and 13:20:16 v0.2.209;
- 2026-10-01 14:23:52 v0.2.210;
- 2026-10-02 03:31:39 to 03:46:27 v0.2.211 and v0.2.210 alternating,
  ending on v0.2.210 (pid 3563663);
- 2026-10-02 05:50:47 v0.2.211 (pid 1012103);
- 2026-10-02 06:24:47 **v0.2.218** (pid 1379292, `confirming update
  v0.2.211 → v0.2.218: start 1 of 3`) — running at the read,
  2026-10-02T06:30Z.

The releases carrying this goal's node work: v0.2.212 (#544), v0.2.214
(#548), v0.2.215 (#555) and **v0.2.218** (#560, 9219031; `git tag
--contains 9219031 | sort -V | head -1`), published
2026-10-02T06:22:22Z. The mini never ran v0.2.212–217: **its first
start carrying any of this goal's node work is v0.2.218, at
2026-10-02T06:24:47Z**, and every session it ran before that ran on
the old default. At 06:25:23Z it adopted two build sessions the
v0.2.211 process had started (this item's own among them, whose
context was written on the old default: 13 files); their records are
not after records.

## Before

From this branch's binary — the installed `/home/chris/bin/mesh` is
v0.2.211 and has no documents-fetched columns:

```sh
go build -o /tmp/mesh ./cmd/mesh/ && /tmp/mesh node usage --from 2026-09-30 --to 2026-10-01
```

```
window 2026-09-30..2026-10-01  records 2026-09-30T00:01:13..2026-09-30T07:25:17
kind      sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes  documents fetched median  fetched by tool
build     137       1282352              2530352            97.2          309796                137                 -                         0
evaluate  181       730895               1032461            93.6          306517                181                 -                         0
rebase    7         353824               725011             95.3          243929                7                   -                         0
plan      908       0                    20159              85.3          41717                 908                 -                         0
research  3         99567                141267             86.7          304351                2                   -                         0
unparsed 0
```

**Before: 137 builds, median input 1,282,352 tokens per build (mean
2,530,352), 97.2% cache reads, median context 309,796 bytes.** The
records span 2026-09-30T00:01:13 to 07:25:17 — about seven and a half
hours of the day, not all of it — and the next record is
2026-10-01T04:29:40. That gap is not a rotation gap: node.log.1 holds
the whole stretch without a break (25,620 lines between the two
records), but no session finished in it. The node logged `accounts: no
account has headroom …` 2,098 times there, and the 4,248 `context …
bytes` lines are sessions it prepared and could not start (`no issued
credential has headroom for the session`): both subscriptions'
seven-day windows were spent, as the statement says. Of the day's 908
plan records, 879 carry 0 input tokens (00:01:13 to 07:17:49Z: planning
sessions that ended without a turn), so the plan median reads 0.

The second before day, the same binary, `--from 2026-10-01 --to
2026-10-02` (still on v0.2.209/v0.2.210, the old default):

```
window 2026-10-01..2026-10-02  records 2026-10-01T04:29:40..2026-10-01T15:46:10
kind      sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes  documents fetched median  fetched by tool
build     56        609039               2243825            97.1          339176                56                  -                         0
evaluate  52        506426               641012             89.3          337368                52                  -                         0
rebase    10        220118               308445             91.3          243146                10                  -                         0
plan      18        514435               538193             86.6          336672                18                  -                         0
research  1         627526               627526             92.2          316637                1                   -                         0
unparsed 0
```

The statement's figures — about 2.9M median input per build, 124
builds, 355M input tokens — came from an earlier read of a fuller log;
the tool's median, 1,282,352 over 137 builds, is the number this
document judges by. The before day already sits above the 1M line;
the bar the after day must clear is 1,000,000 all the same, and 80,000
context bytes against 309,796.

## After

**Owed.** The after window opens at the mini's first node.log start
line of v0.2.218 or any later release, v0.2.218 being the first
release carrying #560; before that start no session writes the
inverted context. That start is

```
mesh node: 2026/10/02 06:24:47 mesh node v0.2.218, pid 1379292, config /home/chris/.mesh/node.yaml
```

At the read, 2026-10-02T06:30Z, five minutes of it have passed, not a
day. The reading is the first full day after that start, 2026-10-03,
so every record falls after it (`mesh node usage` takes whole local
days, and the mini's local time is UTC):

```sh
go build -o /tmp/mesh ./cmd/mesh/ && /tmp/mesh node usage --from 2026-10-03 --to 2026-10-04
grep -a 'context [0-9]* bytes in' ~/.mesh/node.log | awk '$3 == "2026/10/03"'   # the per-session lines; note the max bytes
```

Today's partial reading, from the start to the read: the first three
per-session context lines on v0.2.218, sessions the new process
started itself —

```
mesh node: 2026/10/02 06:26:14 item a-re-land-keeps-its-dependents-and-a-lineage-has-a-cap/1: context 16825 bytes in 1 files, 709 documents to fetch
mesh node: 2026/10/02 06:26:16 item a-session-starts-with-a-real-budget/6: context 17198 bytes in 1 files, 709 documents to fetch
mesh node: 2026/10/02 06:26:17 item a-node-has-an-owner/41: context 26969 bytes in 1 files, 709 documents to fetch
```

— max 26,969 bytes, under the 40KB target and the 80KB cap, against
309,796 median before; the other lines are `project purser: context 0
bytes in 0 files`. One usage record has landed since the start (an
evaluation of `a-node-has-an-owner/41`, 06:28:51Z): too few for a
median, and no build. Today's earlier sessions (2026-10-02, records
02:29:34..06:22:59Z: 115 builds, median input 1,170,083, median
context 276,351 bytes) are the old default and are not an after
reading.

The verdict, on that build row: median input per build under
1,000,000 and median context bytes under 80,000 means the change
held; if either does not, the change is wrong, and this document says
so in those words. **Verdict: owed.**

## Log

- 2026-09-29 · context-budget-that-holds items 1–4, 9, 10 merge on
  mesh-cli (#421, #423, #422, #429, #464, #463); items 7 and 13 on
  mesh-specs (#101, #107) merge too.
- 2026-09-30 · the reading on the mini (v0.2.209): build context 309KB
  median, INDEX.md 190KB, 124 builds at 355M input tokens; from 07:25Z
  no account has headroom and no session finishes until 2026-10-01
  04:29Z.
- 2026-10-01 · Chris asks why the MCP server did not keep the
  filesystem small; context-budget-that-holds is cancelled with items
  17–19 unlanded; this goal is set at 07:25:22Z.
- 2026-10-02 04:26Z · item 2 merges (#544, v0.2.212).
- 2026-10-02 04:29Z · item 1 fails its third evaluation; its work goes
  to item 6.
- 2026-10-02 · item 4 merges (mesh-specs #126), item 8 (mesh-specs
  #129).
- 2026-10-02 05:03Z · item 3 merges (#548, v0.2.214); 05:18Z item 7
  (#555, v0.2.215).
- 2026-10-02 05:50:47Z · the mini starts v0.2.211.
- 2026-10-02 06:00:01Z · item 6 merges (#560, 9219031); v0.2.218, the
  first release carrying it, is published at 06:22:22Z.
- 2026-10-02 06:10Z–06:12Z · item 9 fails: its first document's
  `title:` did not parse, its retry's credential read timed out.
- 2026-10-02 06:24:47Z · the mini starts v0.2.218: the after window
  opens; its first context lines read 16,825–26,969 bytes.
- 2026-10-02 · item 10, this document: the before reading
  (1,282,352 median input per build, 309,796 median context bytes),
  the second before day, the gap named; the after window open from
  06:24:47Z, its reading and the verdict owed until 2026-10-03 has
  passed.
