---
summary: Every document a session's context puts on disk counts against node.yaml's `context.bytes_max`, INDEX.md's documents tables reserved first — instructions and ADRs are exempt from `doc_bytes_max` only, never truncated — chosen in order the instructions and ADRs of the session's own repos, then the other binding documents by recency, then the PRD and TRD; one that does not fit is skipped whole, listed in INDEX.md to fetch with `mesh_document`, and kept on `Context.Dropped`.
status: accepted
---

# ADR-015: The context budget holds

2026-09-29, goal `context-budget-that-holds` (item 2). Amends the
project's context on disk (mesh-specs adr-003 decision 3) as this node
implements it in `internal/node/workspace.go` (`FetchContext`).

## Context

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

## Decision

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

## Consequences

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

## Amendment 2026-09-29: INDEX.md is inside the budget

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
