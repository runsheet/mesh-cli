---
summary: A session's context holds only what it cannot do without — its own repos' instructions, keyed by repo and path, and its own goal with its items — under a 40KB target and an 80KB cap on the bytes written (`context.bytes_max`), stale context always cleared; INDEX.md is a screen with no document table and no other goal, saying how to fetch the rest with `mesh_search`, `mesh_document`, `mesh_documents`, `mesh_projects` and `mesh_items`, the ADRs and TRD binding and searched first.
status: accepted
---

# ADR-015: The context budget holds

2026-10-02, goal `context-is-fetched-not-shipped` (items 1 and 6), which
supersedes `context-budget-that-holds`. Rewrites this ADR's
2026-09-29 version (kept below as history) and amends the project's
context on disk (mesh-specs adr-003 decision 3, trd §3) as this node
implements it in `internal/node/workspace.go` (`FetchContext`,
`RenderIndex`, `WriteContext`).

## Context

The 2026-09-29 version made the budget hold — every document and
INDEX.md inside `bytes_max`, 240KB — but the budget was the problem.
Measured on the mini on 2026-09-30 (node v0.2.209): a build's
`.mesh/context/` was 309KB median, 430KB at worst; INDEX.md alone was
190KB, of which 116KB was two document tables of 1,003 rows and some
70KB a live section listing every live goal's items; nine ADRs rode
along at 120KB whether the item touched them or not. Every prompt says
to read INDEX.md first, so the first turns pulled some 75K tokens into
the transcript and every later turn re-read them: builds averaged 81–99K
input tokens a turn, the large ones 12–14M tokens each, and 124 builds
spent 355M input tokens that day. The MCP tools that were meant to
fetch the rest on demand were served and, in most sessions, never
called.

The on-disk set had swallowed the on-demand one. A document on disk is
paid for on every turn of the session; a document fetched is paid for
once, and only by a session whose work touches it.

## Decision

**What is on disk.** Only what a session cannot do without:

1. the instructions documents (`kind: instructions` — AGENTS.md,
   CLAUDE.md as mesh-context indexes them) whose `repo` is one of the
   session's own repos (`ContextOptions.Repos`, owner/name, compared
   case-insensitively). A build, an evaluation and a rebase pass the
   item's repo; the planner and the director pass every repo of the
   project; `mesh goal` passes none, and channel mode's `mesh_context`
   the project's repos. Another repo's instructions, every ADR, the PRD,
   the TRD, skills and runbooks are never fetched, let alone written:
   `FetchContext` reads their bodies not at all. **Instructions are
   keyed by repo and path, not by slug**: every repo's AGENTS.md is
   slug `agents`, and the gateway's by-slug read
   (`/documents/<slug>`, `mesh_document`) answers one of them — in the
   runsheet/mesh project, mesh-cli's for all three. A slug only one
   instructions document carries is read from the gateway; a shared
   one is read from the session's own checkout of its repo
   (`ContextOptions.Checkouts`, the file at the document's `path`; the
   item's checkout for a build, an evaluation or a rebase, each
   `repos/<name>` for the planner and the director), and with no
   checkout it is left off disk, named in INDEX.md by repo and path to
   read there. Each is written as its own repo's file,
   `instructions-<owner>-<repo>-<slug>.md`, and `Context.OnDisk` is
   keyed by that file name;
2. the session's own goal (`ContextOptions.Goal`), in INDEX.md: its
   statement, acceptance and a table of its items (`seq | title |
   status | repo | branch`). A build, an evaluation and a rebase pass
   the item's goal, the planner the goal it plans; the director has no
   goal and puts none on disk — it reads goals through `mesh_goals`.
   No other goal, live or finished, is named.

The project's listing (`Context.Index`) is still read, for the count
line and for `ContextWritten.ToFetch`; it is never rendered as rows.

**INDEX.md is a screen, not a table.** `RenderIndex` writes: the
project; `On disk here` — each instruction file by name and the repo it
came from, and the goal section; `Binding, not on disk — fetch first` —
any own instructions the cap left off (below); `Everything else: fetch
it` — `mesh_search` (project, query → slug, title, kind, summary),
`mesh_document` (project, slug → body), `mesh_documents` and
`mesh_projects` for another project, `mesh_items` for live state, under
"search before you read, and read a document only when the work touches
it"; one line naming the ADRs and the TRD as binding, to be searched
before any design decision; and one count line, `<n> documents indexed,
none listed here.` Then the goal. No `| kind | title |` table, no row
per document.

**The budget.** node.yaml's `context.bytes_max` (`ContextConfig`,
re-read each tick) is the cap, default **80KB**
(`DefaultContextBytesMax`): every file written, INDEX.md with its goal
section included. INDEX.md's length is counted first; each own
instructions document is then placed in the index's order while the
documents and the index as it will be written (it names the file) stay
within the cap, the index giving way first: the instructions are
binding and the goal's items can be asked for. One that would pass it —
or that alone passes `context.doc_bytes_max` (default 60KB) — is left
off disk whole — never truncated — kept on `Context.Dropped`, and named
in INDEX.md as binding, to fetch with `mesh_document` (by repo and
path, for a shared slug) before the session starts work. An INDEX.md
past the cap at its shortest places nothing, with one warning
(`warning: INDEX.md alone is <n> bytes at its shortest, past bytes_max
<m>: …`).

The cap holds on what is written, not only on what was counted.
`RenderIndex` never returns more than the room the instruction files
leave under the cap: past it the goal's item table is cut at a row
boundary — as many whole rows as fit, then `… <n> more items past the
context cap (seq <s> on): ask mesh_items for the rest.` — then its
statement and acceptance give way to the same pointer, and an index
still past it — a cap smaller than its own lines — is cut at the cap
with a closing line naming `mesh_items` and `mesh_search`. A dropped
document's line grows the index after the documents before it were
placed, so `FetchContext` fits again at the end, and `WriteContext`
fits once more before it writes (`Context.fit`): while the documents
and the index as rendered pass the cap, the last instruction file by
name leaves the disk for `Dropped`. `WriteContext` then
measures INDEX.md and every file as rendered, the goal section and the
dropped lines included, before it writes a byte, and refuses a context
past the cap rather than write it: nothing over the cap is ever
written. What `ContextWritten.Bytes` reports is what is on disk after
the call, and it is never past the cap
(`TestOwnGoalItemTablePastTheCapIsCutAtARow`).

The **target is 40KB** (`ContextBytesTarget`). A session whose context
passes it still runs, and logs once, after its context line,
`warning: context <n> bytes, past the 40960-byte target: <file> took it
past` (`ContextWritten.TargetWarning`), the file being the one whose
bytes crossed the target in writing order (the instruction files by
name, then INDEX.md).

`context.doc_bytes_max` holds each instructions document and nothing
else: no design document is put on disk any more.

What is on disk is what this session wrote. A checkout is reused
across attempts, so `WriteContext` clears `.mesh/context` before it
writes anything — an empty context too, which is what a session gets
when the context read fails: an earlier node's ADR bodies or an
INDEX.md past the cap never ride into a retry, and its `0 bytes` is
true (`TestContextReadFailureClearsAReusedCheckout`).

The per-session line is unchanged (ADR-016): `context <bytes> bytes in
<files> files, <n> documents to fetch`, and `contextBytes` on the usage
report is the same `Bytes`.

**Worked example.** A project of 1,000 indexed documents — ADRs, the
PRD and TRD, skills, and three repos' AGENTS.md, all slug `agents` —
and 12 live goals of 20 items. A session in two of the three repos,
`acme/widgets` and `acme/gears`, writes
`instructions-acme-gears-agents.md` and
`instructions-acme-widgets-agents.md` (15KB each, each its own repo's
body read from its checkout, not the gateway's one answer for
`agents`) and an INDEX.md under 5KB holding its own goal's 20 items:
about 34KB in all, under 40KB, `998 documents to fetch`, no ADR on
disk, none of the other 11 goals named
(`TestContextHoldsOnlyInstructionsAndOwnGoal`). Were that AGENTS.md
100KB it would stay off disk, INDEX.md naming it as binding, to fetch
first, and the context would be the index alone
(`TestContextNeverPastTheCap`).

## Consequences

- A session reads its ADRs, the PRD and the TRD by searching and
  fetching. They are still binding: INDEX.md says so and says to search
  them before any design decision. A session that does not fetch a
  binding ADR its work touches is wrong in the same way one that did
  not read the file was.
- The cost of a context no longer grows with the estate's document
  count or its number of live goals: it is the own repos' instructions
  plus one goal. A project whose AGENTS.md alone passes 40KB is told so
  every session, which is the prompt to trim it.
- The prompts still point at INDEX.md first; the planner naming in
  each item's brief the documents a builder must fetch, and bounded
  `mesh_search`/`mesh_document` bodies, are the same goal's other
  items — this ADR moves the bytes, those make the fetches cheap.
- Live state of other goals is a `mesh_items` call away rather than on
  disk; the planner's re-plan still gets its own goal's items in its
  prompt.
- Owed to runsheet/mesh-specs: trd §3 (the context on disk) and
  adr-003 decision 3 (INDEX.md carries the live goals and items) are
  amended there for this default.

## History: the 2026-09-29 version

Goal `context-budget-that-holds`, items 2 and 10. Superseded by the
decision above; kept as it was decided.

2026-09-29, goal `context-budget-that-holds` (item 2). Amends the
project's context on disk (mesh-specs adr-003 decision 3) as this node
implements it in `internal/node/workspace.go` (`FetchContext`).

### Context

On 2026-09-28 the estate ran 500 sessions on 1.16 billion input tokens
against 7.3 million output — 160 to 1, 97% of it cache reads. Almost
none of that input is the model thinking: it is the same context,
re-read on every turn of every session, and the plan windows it spends
are the real currency on a subscription.

The node had a size budget — 240KB in total, 60KB per design document
— but instructions and ADRs were exempt from it entirely: a binding
document went on disk whatever its size and whatever the running
total. The real ceiling was 240KB plus every AGENTS.md and every ADR
of the project, without limit, and it grew with every ADR the estate
wrote (most days), for every session of every kind.

### Decision

**The keys.** node.yaml's `context:` block (item 1, `ContextConfig`,
re-read each tick): `bytes_max`, the total a session's context may put
on disk (default 240KB), and `doc_bytes_max`, the most one design
document may (default 60KB). Instructions and ADRs are exempt from
`doc_bytes_max` — they are binding, and truncating one is worse than
leaving it out — but **not** from `bytes_max`.

**The order.** `FetchContext` fetches every document of the kinds it
reads (instructions, adr, prd, trd), then walks them in three tiers:

1. the instructions, then the ADRs, whose `repo` is one of the
   session's own repos (`ContextOptions.Repos`, owner/name);
2. the other instructions, then the other ADRs, the most recent first —
   the index carries no date, so recency is the number in the slug
   (`adr-014` before `adr-001`), a slug with none last;
3. the PRD and TRD, as before, each within `doc_bytes_max` too.

A document that would take the running total past `bytes_max` is
skipped whole and the walk goes on, so a smaller one later may still
land. A skipped document is never written in part: INDEX.md lists it
as `fetch`, and a skipped binding one is on `Context.Dropped` (for the
per-session log line).

Whose repos: a build, an evaluation and a rebase pass the item's repo;
the planner and the director, which work project-wide, pass every repo
of the project (`ProjectRepos`); `mesh goal` and channel mode pass none.

**Worked example.** `bytes_max: 3000`; the item works in
`acme/widgets`; the project holds `acme/widgets`' AGENTS.md and
`adr-001`, and `adr-002` and `adr-004` of `acme/other` and `adr-003`
of `acme/third`, 1000 bytes each. Tier 1 puts the widgets AGENTS.md
and `adr-001` on disk (2000). Tier 2 walks `adr-004`, `adr-003`,
`adr-002`: `adr-004` fits (3000), the other two would pass 3000 and are
dropped — rows ending `| fetch |` in INDEX.md. With `bytes_max: 1200`
and the widgets documents 500 bytes each, both still land, a 5000-byte
`adr-011` elsewhere is skipped, and a 150-byte `adr-002` after it
fits in what is left.

### Consequences

- A session may find a binding ADR only in the index. It is still
  binding: the session fetches it with `mesh_document` (the prompts
  already point there) when its work touches it.
- The item's own repo always wins: its AGENTS.md and ADRs are the
  first bytes placed, so a project whose binding documents alone
  exceed the budget still yields a context led by them. Only an own
  repo's documents larger than `bytes_max` together leave some of
  those to the index too.
- The ceiling is now `bytes_max`, whatever the estate writes; an
  operator trims it in node.yaml without a release.
- Recency by slug number is a proxy. When the index carries a date,
  the order can use it without changing the tiers.

### Amendment 2026-09-29: INDEX.md is inside the budget

Goal `context-budget-that-holds`, item 10. On the mini a session's
context line read `context 443252 bytes in 16 files, 667 documents to
fetch` against the 240 KB default, and on a planning session's disk
INDEX.md alone was 165 KB of 383 KB: the documents held `bytes_max`,
the index beside them did not, so the budget did not hold on disk.

**The index is reserved first.** Before any document is placed,
`FetchContext` renders INDEX.md's documents tables with nothing on disk
and reserves that length from `bytes_max`; documents are placed in the
tiers above against `bytes_max` less the reserve. An on-disk row names
its file where a row to fetch carries a summary, so the index as it
will be written can be a little larger than the reserve: it is
rendered again after placement and, while the documents and it
together pass `bytes_max`, the last-placed document goes back to the
index (reverse placement order: the PRD and TRD, then the other
binding documents, then the own repos'), a binding one onto
`Context.Dropped`. Nothing is truncated and the own-repo tier still
wins. An index alone past `bytes_max` places no document at all and
says so once in the session's log (`warning: INDEX.md alone is <n>
bytes, past bytes_max <m>: …`); the table is never capped — every
document stays one row. `Context.IndexBytes` is the rendered tables'
length.

**Two tables.** `Documents a session reads` holds the `ContextKinds`
documents (instructions, adr, prd, trd) with `kind | title | what it
says | slug | repo/path | here`, the summary only when the document is
not on disk. `Other documents` holds every other kind with `kind |
title | slug` alone: `FetchContext` never puts those kinds on disk, so
the `here` cell always said `fetch`, and `mesh_document` takes the
slug, so the path is not needed to fetch one. They are about 560 of
770 rows on the runsheet project, and the path and `here` cells were
some 80 bytes a row. The counts line after the tables names both:
`27 documents on disk here, 656 to fetch (102 a session reads, 554
other); summaries are shown only for …`.

**The live section is outside the budget.** `## Live goals and items`
(`AddLive`) is read after the documents are placed, so its size is
known only after the fetch; it follows the tables unchanged and is not
counted against `bytes_max`. The per-session log line and the usage
record's `contextBytes` (ADR-016) still report every byte written,
INDEX.md with its live section included.
