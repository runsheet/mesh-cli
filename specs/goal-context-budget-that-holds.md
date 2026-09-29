---
title: Goal — A session's context has a budget that actually holds, and the node reports what it cost
kind: plan
status: draft
parent: prd
summary: The node's reading (mesh node usage over the mini's node.log, 2026-09-22..2026-09-28) puts the before median at 1,114,518 input tokens per build session over 121 paired build sessions; the after reading is not yet due (2026-10-07), and the platform's mesh-bench.py reading is the operator's and is owed.
---

# Goal — A session's context has a budget that actually holds, and the node reports what it cost

Mesh goal `context-budget-that-holds`, running.

## Statement

A day of Mesh on 2026-09-28: 500 sessions, 1.16 billion input tokens against 7.3 million output — 160 to 1 — with 97% of the input cache reads. The plan windows are the real currency on a subscription, and one account is already at 83% of its seven-day window with work left unclaimed for lack of headroom. Almost none of that is the model thinking. It is the same context, re-read every turn of every session.

`internal/node/workspace.go` has the budget, and the budget does not hold:

- `ContextBytesMax = 240 * 1024` and `DocBytesMax = 60 * 1024` are **compile-time constants**. An operator cannot trim them without a release, so the one lever that touches every session kind at once cannot be pulled.
- **Instructions and ADRs are exempt from the budget entirely** — `binding := d.Kind == "instructions" || d.Kind == "adr"`, and a binding document is written to disk whatever its size and whatever the running total. So the real ceiling is 240KB *plus every ADR and every AGENTS.md in the project, without limit*. This grows every time the estate writes an ADR, which it does most days, and it grows for every session of every kind. That is the leak.
- `RenderIndex` writes `INDEX.md` listing **every** indexed document of the project with kind, title, summary, slug and path. It is a table that grows linearly with the estate and is injected whole, in every session.

Make the budget hold:

- **Configurable.** `context:` in node.yaml, read again each tick the way capacity and the accounts are: `bytes_max` and `doc_bytes_max`, the current values as defaults so nothing changes on upgrade.
- **Bound the binding set.** Instructions and ADRs stay exempt from `doc_bytes_max` — they are binding and truncating one is worse than dropping it — but they must not be exempt from the *total*. Prefer the ones the item touches: the AGENTS.md and ADRs of the repos in the item's own work, then project-wide ones, then the rest by recency until the total is reached. What does not fit is listed in the index and fetched on demand with `mesh_document`, which already works.
- **Trim the index.** One row per document is fine; the summary is what makes it heavy. Keep the summary for the kinds a session is likely to fetch and drop it for the rest, or cap the table and say how many were omitted.
- **Say what it cost.** Log the bytes written and the count dropped, per session, and put the bytes on the usage record so the benchmark can see the effect rather than infer it.

The measurement matters as much as the change: `mesh-bench.py` reports cost per merged item and input tokens per session. Take a reading before and after on the same repositories, and put both in the goal doc. If input tokens per build session do not fall, the change was wrong and should be reverted rather than kept for tidiness.

## Acceptance

`go vet ./...` clean, `gofmt -l .` empty, `go build ./cmd/mesh/` and the linux/amd64 cross-build fine, `go test ./...` green.

Config: `context: {bytes_max, doc_bytes_max}` loads from node.yaml, is re-read by `Reload` without a restart (a test proves the new value is in force on the next tick), and its absence keeps today's 240KB and 60KB exactly.

Budget: a test with more binding documents than the total budget proves the total is honoured — the item's own repos' instructions and ADRs are on disk, the rest are in the index and absent from disk — and that no binding document is ever truncated mid-file. A project whose binding documents alone exceed the budget still produces a usable context: the item's own repo wins.

Index: `INDEX.md` renders every document as a row, and the assertion for the trimmed summaries is exact (the tests here assert whole lines, per AGENTS.md).

Reporting: the node logs one line per session with the bytes written and the number of documents left to fetch, and the bytes ride the usage record.

Evidence in the goal doc: `mesh-bench.py` output from before the change and after it, on the same project, with median input tokens per build session in both. Name the number that moved.

## Items

As `mesh_items` (project `runsheet/mesh`, goal `context-budget-that-holds`) records them at 2026-09-29T14:45Z.

| Seq | Title | Status | PR | Cost |
| --- | --- | --- | --- | --- |
| 1 | Node: `context:` in node.yaml — bytes_max and doc_bytes_max, read again each tick like the accounts, defaults 240KB and 60KB; FetchContext takes the budget instead of constants | merged | [mesh-cli#421](https://github.com/runsheet/mesh-cli/pull/421) | $2.35 |
| 2 | Node: instructions and ADRs count against the total — the item's own repos first, then the rest by recency; what does not fit is in the index, never truncated; ADR-015 | merged | [mesh-cli#423](https://github.com/runsheet/mesh-cli/pull/423) | $1.63 |
| 3 | Node: INDEX.md keeps one row per document but carries a summary only for the kinds a session fetches and that are not on disk | merged | [mesh-cli#422](https://github.com/runsheet/mesh-cli/pull/422) | $2.63 |
| 4 | Node: one log line per session with the context bytes written and the documents left to fetch, and `contextBytes` on the usage record with the older-platform fallback; ADR-016 | merged | [mesh-cli#429](https://github.com/runsheet/mesh-cli/pull/429) | $5.23 |
| 5 | Goal doc: context-budget-that-holds — the reading before and after on the same project, the number that moved, what landed and what is owed; ADR-015 records the index trim and the reporting | failed — three evaluations: its "before" was a cost-paired sample of node.log presented as the reading, and its diff carried Go changes | — | $7.15 |
| 6 | Node: every session's usage record is logged on its label with its kind, whatever became of the report, and `mesh node usage` reads node.log into the reading — sessions, median and mean input tokens, cache-read share and median context bytes by kind, for a window; ADR-016 amended | review | [mesh-cli#448](https://github.com/runsheet/mesh-cli/pull/448) | $3.09 |
| 7 | mesh-specs: the TRD's context-on-disk paragraphs and adr-003 name the context budget — node.yaml `context:`, the binding order, never truncated, the trimmed index, the per-session log line and `contextBytes` owed on UsageReportInput | review | [mesh-specs#101](https://github.com/runsheet/mesh-specs/pull/101) | $1.65 |
| 8 | Goal doc: context-budget-that-holds — the node's reading before and after from `mesh node usage` on the same project with its coverage stated, the platform's mesh-bench.py reading named as the operator's and owed, what landed and what is owed | running (this document) | — | — |

## Measurement

**The reading the goal names is not one this project can take.** `mesh-bench.py` is in none of this project's repositories (mesh-cli, mesh-portal, mesh-specs), and the gateway schema (`internal/gateway/schema.json`) has the `recordUsage` mutation but no query that returns usage records, so neither a session nor the node can read the platform's ledger back. The platform's mesh-bench.py reading — cost per merged item and median input tokens per build session — is the operator's to take on the platform, and it is **owed** (see Owed). No number in this document is mesh-bench.py's.

What this project can take is the node's own reading: `mesh node usage` (item 6, ADR-016's amendment) over this host's `~/.mesh/node.log` — the node on this machine, the mini, ran the sessions. Item 6 is on this branch's base and not yet in a release, so the command is built from the checkout: `go build ./cmd/mesh/ && ./mesh node usage --log ~/.mesh/node.log …`. Everything below is **the node's reading**, taken 2026-09-29T14:45Z.

How the node's reading counts: every record in the window is a legacy `usage: {…}` line (no released node yet writes item 6's `<label>: usage <kind> {…}`), so the command gives a record a kind only when it pairs it, by cost, with a session's end line nearby; a record it cannot pair is `unknown`. The `build` row is therefore the paired build sessions, not every build session.

### The first context line

`grep -an 'context [0-9]* bytes in' ~/.mesh/node.log | head -1` finds line 6190447, `+  item <goal>/<seq>: context 187392 bytes in 21 files, 43 documents to fetch` — a diff a session printed into the log, not a line the node wrote. Anchored to the node's own prefix:

```sh
grep -anE '^mesh node: [0-9/]+ [0-9:]+ .*context [0-9]+ bytes in' ~/.mesh/node.log | head -1
```

```text
6282066:mesh node: 2026/09/29 14:13:56 item evaluation-reports-no-verdict-platform/11: context 0 bytes in 0 files, 0 documents to fetch
```

That is the node's first session on `v0.2.191` (started 14:13:19), the first release to run here carrying items 1–4 (items 1, 2 and 4 are in `v0.2.189`, item 3 in `v0.2.190`). The node went back to `v0.2.188` at 14:14:06 on a stale desired version and returned to `v0.2.191` at 14:25:00, where it has stayed.

### Before

The log begins 2026-09-21T04:11 and the first context line is 2026-09-29T14:13, so the full days it covers before that line are 2026-09-22 through 2026-09-28:

```sh
./mesh node usage --log ~/.mesh/node.log --from 2026-09-22 --to 2026-09-29 --kind build
```

```text
window 2026-09-22..2026-09-29  records 2026-09-28T11:43:51..2026-09-28T23:57:16
kind   sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes
build  121       1114518              2333752            97.2          -                     0
unparsed 0
```

The coverage, as the command reports it — the records it could not pair, over the same window:

```sh
./mesh node usage --log ~/.mesh/node.log --from 2026-09-22 --to 2026-09-29 --kind unknown
```

```text
window 2026-09-22..2026-09-29  records 2026-09-25T23:53:26..2026-09-28T23:58:51
kind     sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes
unknown  1815      307404               908461             95.6          -                     0
unparsed 0
```

and the build sessions the node finished in the window, one `item …: finished …($x)` line each:

```sh
grep -aE '^mesh node: 2026/09/2[2-8] ' ~/.mesh/node.log | grep -acE 'item [^ ]+: finished .*\(\$[0-9.]+\)'
```

```text
2020
```

So the node's before reading stands on 121 paired build sessions, beside 1815 records it could not pair and 2020 finished builds in the window: about 6% of the window's build sessions, all of them from 2026-09-28 11:43 on (the log carries no usage record before 2026-09-25T23:53, and pairs none as a build before 2026-09-28T11:43). It is the node's reading of the last day before the change, not of the week.

### After

The window from the first context line to now:

```sh
./mesh node usage --log ~/.mesh/node.log --from 2026-09-29 --to 2026-09-30 --kind build
```

```text
window 2026-09-29..2026-09-30  records 2026-09-29T00:05:30..2026-09-29T14:11:48
kind   sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes
build  382       1126594              2238070            97.1          -                     0
unparsed 0
```

The command reads whole local days, and every one of these build records is from before the first context line (the last is 14:11:48, the line 14:13:56): **no build session after the change is in the build row — zero, fewer than 20 — so the after reading is not yet due.** No context bytes median is on the row for the same reason. The sessions after 14:13:56 are in the `unknown` row so far, where 58 records carry `contextBytes`; `v0.2.191` does not write item 6's labelled record, so the build row fills from them only as the cost pairing finds their end lines, or fully once a release carrying item 6 runs on the mini.

Seven days from the first context line is 2026-10-06T14:13. Because the command counts whole days and 2026-09-29 is mostly before the line, the after reading is **due on 2026-10-07**, over the seven whole days after it, with:

```sh
go build ./cmd/mesh/ && ./mesh node usage --log ~/.mesh/node.log --from 2026-09-30 --to 2026-10-07 --kind build
```

Then name the number that moved — median input tokens per build session, before → after — and apply the goal's rule: if it did not fall, the change is reverted.

## The numbers

The node's reading, from the blocks above. None of these numbers is mesh-bench.py's.

| Reading | Sessions | Median input tokens | Mean input tokens | Cache read % | Context bytes median | Coverage |
| --- | --- | --- | --- | --- | --- | --- |
| before — the node's reading, 2026-09-22..2026-09-28 | 121 | 1,114,518 | 2,333,752 | 97.2 | — (not yet logged) | 121 build rows beside 1815 unknown and 2020 finished builds; records from 2026-09-28T11:43 |
| after — the node's reading, 2026-09-30..2026-10-06 | not yet due (0 build sessions after 2026-09-29T14:13:56) | — | — | — | — | due 2026-10-07 |

## Owed

- **runsheet/runsheet: `UsageReportInput.contextBytes`** — an `Int` stored on the usage record (ADR-016). Until it lands each report is refused with the field and resent without it, so the bytes are only in node.log.
- **The platform's mesh-bench.py reading, by the operator.** The exact ask: cost per merged item and median input tokens per build session for project `runsheet/mesh`, over the seven days before and the seven days after the first node release carrying items 1–4, **`v0.2.190`** (`gh release list --repo runsheet/mesh-cli --limit 10`: `v0.2.189` 2026-09-29T13:50Z carries items 1, 2 and 4, `v0.2.190` 2026-09-29T14:03Z adds item 3; the mini first ran it as `v0.2.191` at 2026-09-29T14:13Z). This is the reading the goal's acceptance names; this document does not stand in for it.
- **The node's after reading** on 2026-10-07 by the command in Measurement, and the revert if median input tokens per build session did not fall.
- **Item 6 in a release.** Until `<label>: usage <kind> {…}` (mesh-cli#448) runs on the mini, the node's reading covers only the records the cost pairing finds.
- **Nodes on an older binary: none.** `mesh_health_nodes` for `runsheet/mesh` at 2026-09-29T14:43Z lists one node, the mini, on `v0.2.191` with desired version `v0.2.191` — past `v0.2.190`.

## Log

- 2026-09-29 12:04Z — item 1 merged, mesh-cli#421: node.yaml `context:`.
- 2026-09-29 12:27Z — item 2 merged, mesh-cli#423: binding documents count against the total; ADR-015.
- 2026-09-29 12:59Z — item 4 merged, mesh-cli#429: the per-session context line and `contextBytes`; ADR-016.
- 2026-09-29 13:26Z — item 3 merged, mesh-cli#422: the trimmed index.
- 2026-09-29 14:10Z — item 5 failed its third evaluation: a cost-paired node.log sample presented as the reading, and Go changes in a docs item.
- 2026-09-29 14:13Z — the mini starts `v0.2.191` and logs its first context line.
- 2026-09-29 14:39Z — item 6 in review, mesh-cli#448: labelled usage records and `mesh node usage`.
- 2026-09-29 — item 7 in review, mesh-specs#101: the TRD and adr-003 name the budget.
- 2026-09-29 14:45Z — item 8: this document, with the node's before reading; the after reading due 2026-10-07; the platform reading owed.
