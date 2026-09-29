---
title: A session's context has a budget that actually holds, and the node reports what it cost
kind: plan
status: draft
parent: prd
summary: Before, 138 build sessions of 219 (median input 1,119,187 tokens); after is not yet due — 0 records of 38 build sessions since v0.2.191's first context line, rerun 2026-10-06T14:13:56Z; mesh-bench.py is the operator's and owed.
---

# A session's context has a budget that actually holds, and the node reports what it cost

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

**How it stands.** On main (`0d4c95fa4dad479a1eb9d5f58729602190d91cba`): Config holds — `context:` in node.yaml, re-read each tick, 240KB and 60KB by default ([mesh-cli#421](https://github.com/runsheet/mesh-cli/pull/421)). Budget holds — instructions and ADRs count against the total, the session's own repos first, never truncated, ADR-015 ([mesh-cli#423](https://github.com/runsheet/mesh-cli/pull/423)), and INDEX.md itself is inside `bytes_max`, reserved before documents are placed ([mesh-cli#463](https://github.com/runsheet/mesh-cli/pull/463)). Index holds — one row per document, summaries only for the kinds a session fetches and that are not on disk, asserted by whole lines ([mesh-cli#422](https://github.com/runsheet/mesh-cli/pull/422), then two tables in #463). Reporting holds on the node's side — one `context <n> bytes in <k> files, <m> documents to fetch` line per session and `contextBytes` on the usage report, dropped for a platform without the field, ADR-016 ([mesh-cli#429](https://github.com/runsheet/mesh-cli/pull/429)); the platform's `UsageReportInput.contextBytes` is owed, so the bytes do not yet reach the usage record. The design is recorded in mesh-specs ([mesh-specs#101](https://github.com/runsheet/mesh-specs/pull/101); #107 in review). Owed: the evidence clause — the mesh-bench.py reading is the operator's (see Measurement), and the node's own after reading is not yet due; `mesh node usage` with `--project`, timestamps and coverage is in review, not on main (items 9 and 11, [mesh-cli#464](https://github.com/runsheet/mesh-cli/pull/464) and [mesh-cli#466](https://github.com/runsheet/mesh-cli/pull/466)).

## Items

Every item as `mesh_items` (project `runsheet/mesh`, goal `context-budget-that-holds`) recorded it at **2026-09-29T16:15:26Z**. Cost is the item's `cost_cents` in dollars.

| Seq | Title | Status | PR | Cost |
|---|---|---|---|---|
| 1 | Node: `context:` in node.yaml — bytes_max and doc_bytes_max, read again each tick like the accounts, defaults 240KB and 60KB; FetchContext takes the budget instead of constants | merged | [mesh-cli#421](https://github.com/runsheet/mesh-cli/pull/421) | $2.35 |
| 2 | Node: instructions and ADRs count against the total — the item's own repos first, then the rest by recency; what does not fit is in the index, never truncated; ADR-015 | merged | [mesh-cli#423](https://github.com/runsheet/mesh-cli/pull/423) | $1.63 |
| 3 | Node: INDEX.md keeps one row per document but carries a summary only for the kinds a session fetches and that are not on disk | merged | [mesh-cli#422](https://github.com/runsheet/mesh-cli/pull/422) | $2.63 |
| 4 | Node: one log line per session with the context bytes written and the documents left to fetch, and `contextBytes` on the usage record with the older-platform fallback; ADR-016 | merged | [mesh-cli#429](https://github.com/runsheet/mesh-cli/pull/429) | $5.23 |
| 5 | Goal doc: context-budget-that-holds — the reading before and after on the same project, the number that moved, what landed and what is owed; ADR-015 records the index trim and the reporting | failed | none | $7.15 |
| 6 | Node: every session's usage record is logged on its label with its kind, whatever became of the report, and `mesh node usage` reads node.log into the reading — sessions, median and mean input tokens, cache-read share and median context bytes by kind, for a window; ADR-016 amended | cancelled | [mesh-cli#448](https://github.com/runsheet/mesh-cli/pull/448) (closed, carried as item 9) | $3.09 |
| 7 | mesh-specs: the TRD's context-on-disk paragraphs and adr-003 name the context budget — node.yaml `context:`, the binding order, never truncated, the trimmed index, the per-session log line and `contextBytes` owed on UsageReportInput | merged | [mesh-specs#101](https://github.com/runsheet/mesh-specs/pull/101) | $1.65 |
| 8 | Goal doc: context-budget-that-holds — the node's reading before and after from `mesh node usage` on the same project with its coverage stated, the platform's mesh-bench.py reading named as the operator's and owed, what landed and what is owed | failed | none | $3.50 |
| 9 | Node: item 6's work on main — every session's usage record logged on its label with its kind, and `mesh node usage` reads node.log into the reading (PR #448 carried) | review | [mesh-cli#464](https://github.com/runsheet/mesh-cli/pull/464) | $1.99 |
| 10 | Node: INDEX.md is inside the budget — its rows are reserved from bytes_max before documents are placed, the kinds a session never reads are listed in a compact second table, and the bytes on disk never exceed bytes_max; ADR-015 amended | merged | [mesh-cli#463](https://github.com/runsheet/mesh-cli/pull/463) | $2.10 |
| 11 | Node: `mesh node usage` reads a precise window and one project — `--from`/`--to` take RFC 3339 timestamps as well as dates, `--project org/slug` narrows to the project's goals resolved from the platform (or `--goals <file>` offline), and the reading prints its coverage: finished sessions of each kind in the window beside the records read; ADR-016 amended | review | [mesh-cli#466](https://github.com/runsheet/mesh-cli/pull/466) | $2.22 |
| 12 | Goal doc: context-budget-that-holds — the node's before and after readings from `mesh node usage --project runsheet/mesh` with timestamps and coverage, the after window starting at the first context line of the release carrying items 1–4 and 10, the platform's mesh-bench.py reading named as the operator's and owed, every item as mesh_items records it, and what is owed | running | none (this document) | $2.75 |
| 13 | mesh-specs: the TRD's context budget paragraphs say INDEX.md is inside bytes_max, reserved first, in two tables with the live section outside; adr-003 decision 3 amended in a dated line | review | [mesh-specs#107](https://github.com/runsheet/mesh-specs/pull/107) | $2.39 |

## Measurement

**(a) The platform's reading.** `mesh-bench.py` lives in `runsheet/runsheet`, outside this project's repos, so the reading the goal's acceptance names is the operator's to take there, and it is owed (see Owed). The ask: cost per merged item and median input tokens per build session for project `runsheet/mesh`, over the seven days before and the seven days after the first release carrying items 1–4 and 10 (the tag `v0.2.194`, whose release was not yet published at the time of writing — see (b)). The gateway's `schema.json` lists `usageRecord`, `usageIndexs` and `usageWork`, but the node's client reads none of them and their filters are not in the checked-in schema, so the node takes its own reading from node.log instead.

**(b) The node's reading**, taken on the mini's `~/.mesh/node.log` at 2026-09-29T16:17:06Z. `mesh node usage` with `--project`, timestamps and coverage is not yet on main: it is item 11 ([mesh-cli#466](https://github.com/runsheet/mesh-cli/pull/466), in review), so the readings were taken with `mesh` built from its branch, `origin/mesh/context-budget-that-holds-11` at `2f9205011c9c835cf8e1c753af2ed255953d45a5`, on top of main as it stood then:

```
$ git rev-parse origin/main
10a9ba200cd13012ea8d3f291b379659881fb202
```

(Main has since moved to `0d4c95fa4dad479a1eb9d5f58729602190d91cba`, #458, which touches none of the reading.) The mini's node itself runs v0.2.191:

```
$ ~/bin/mesh version
mesh v0.2.191 (commit 8eba662)
```

The releases, to find the first one carrying item 10 (#463, merged as `10a9ba200cd13012ea8d3f291b379659881fb202`):

```
$ gh release list --repo runsheet/mesh-cli --limit 20
v0.2.193	Latest	v0.2.193	2026-09-29T16:09:30Z
v0.2.192		v0.2.192	2026-09-29T14:49:37Z
v0.2.191		v0.2.191	2026-09-29T14:07:40Z
v0.2.190		v0.2.190	2026-09-29T14:03:12Z
v0.2.189		v0.2.189	2026-09-29T13:50:29Z
v0.2.188		v0.2.188	2026-09-29T00:50:45Z
v0.2.187		v0.2.187	2026-09-28T23:28:42Z
v0.1.0	Pre-release	v0.1.0	2026-09-28T07:47:09Z
v0.0.0-runsheet-test.6	Pre-release	v0.0.0-runsheet-test.6	2026-09-26T15:06:03Z
v0.0.0-selfupdate-test.1	Pre-release	v0.0.0-selfupdate-test.1	2026-09-25T13:33:49Z
```

No published release carries item 10: v0.2.193 is `e3e3f97`, #463's parent. release-on-merge tagged `v0.2.194` at `10a9ba2`, but its release-tag.yml run failed, and `v0.2.195` (`0d4c95f`) was still building (its conclusion is empty; the trailing space the command prints after it is dropped here):

```
$ gh run list --repo runsheet/mesh-cli --workflow release-tag.yml --limit 3 --json headBranch,status,conclusion,createdAt -q '.[]|"\(.createdAt) \(.headBranch) \(.status) \(.conclusion)"'
2026-09-29T16:16:05Z v0.2.195 in_progress
2026-09-29T15:51:39Z v0.2.194 completed failure
2026-09-29T15:20:45Z v0.2.193 completed success
```

So the mini has not started a release carrying item 10, and the after reading below is **after items 1–4, before item 10**. The node's first line on every start is `mesh node <version>, pid <pid>, config <path>` (`StartLines`, `internal/node/selfupdate.go`); its starts of v0.2.191:

```
$ grep -anE '^mesh node: [0-9/]+ [0-9:]+ mesh node v0\.2\.19[0-9], pid' ~/.mesh/node.log
6282027:mesh node: 2026/09/29 14:13:19 mesh node v0.2.191, pid 670175, config /home/chris/.mesh/node.yaml
6284803:mesh node: 2026/09/29 14:17:04 mesh node v0.2.191, pid 708121, config /home/chris/.mesh/node.yaml
6295951:mesh node: 2026/09/29 14:21:24 mesh node v0.2.191, pid 755045, config /home/chris/.mesh/node.yaml
6309291:mesh node: 2026/09/29 14:25:00 mesh node v0.2.191, pid 794933, config /home/chris/.mesh/node.yaml
```

Between those starts v0.2.188 ran again (14:14:06, 14:19:33, 14:24:10); `~/.mesh/update.json` confirmed v0.2.191 at 2026-09-29T14:25:31Z and it has run since. The first context line is the first after v0.2.191's first start (node.log's timestamps are the mini's local time, UTC):

```
$ grep -anE '^mesh node: [0-9/]+ [0-9:]+ .*context [0-9]+ bytes in' ~/.mesh/node.log | head -1
6282066:mesh node: 2026/09/29 14:13:56 item evaluation-reports-no-verdict-platform/11: context 0 bytes in 0 files, 0 documents to fetch
```

**Before** — the seven days ending at that line:

```
$ mesh node usage --project runsheet/mesh --from 2026-09-22T14:13:56Z --to 2026-09-29T14:13:56Z --kind build
window 2026-09-22T14:13:56Z..2026-09-29T14:13:56Z  records 2026-09-28T11:57:50..2026-09-29T14:11:48
kind   sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes
build  138       1119187              1820329            96.5          -                     0
coverage build: 138 records of 219 sessions ended in the window
unparsed 0
```

```
$ mesh node usage --project runsheet/mesh --from 2026-09-22T14:13:56Z --to 2026-09-29T14:13:56Z --kind evaluate
window 2026-09-22T14:13:56Z..2026-09-29T14:13:56Z  records 2026-09-28T11:41:15..2026-09-29T14:13:09
kind      sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes
evaluate  131       418105               523584             88.9          -                     0
coverage evaluate: 131 records of 213 sessions ended in the window
unparsed 0
```

node.log's usage records begin 2026-09-28T11:57:50 (build), so the before window's records cover its last day and a quarter; the coverage lines say how many of the window's finished sessions they are.

**After items 1–4, before item 10** — from the first context line to the time of writing:

```
$ mesh node usage --project runsheet/mesh --from 2026-09-29T14:13:56Z --to 2026-09-29T16:15:26Z --kind build
window 2026-09-29T14:13:56Z..2026-09-29T16:15:26Z  records -
kind  sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes
coverage build: 0 records of 38 sessions ended in the window
unparsed 0
```

38 build sessions of runsheet/mesh ended in the window and none of their usage records was read: v0.2.191 logs the unlabelled `usage: {…}` record, and in this window the reader paired none to a session's end line — without `--project` every one of them reads as kind `unknown`:

```
$ mesh node usage --from 2026-09-29T14:13:56Z --to 2026-09-29T16:15:26Z
window 2026-09-29T14:13:56Z..2026-09-29T16:15:26Z  records 2026-09-29T14:15:05..2026-09-29T16:15:05
kind     sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes
unknown  247       0                    69297              96.5          41860                 235
coverage build: 0 records of 81 sessions ended in the window
coverage evaluate: 0 records of 79 sessions ended in the window
coverage rebase: 0 records of 5 sessions ended in the window
coverage plan: 0 records of 14 sessions ended in the window
coverage research: 0 records of 1 sessions ended in the window
unparsed 0
```

The records the after reading needs are the labelled `<label>: usage <kind> {…}` lines of item 9 ([mesh-cli#464](https://github.com/runsheet/mesh-cli/pull/464)); the mini logs them once it runs a release carrying items 9 and 11.

**The rerun**, due seven days after the after window's start, 2026-09-29T14:13:56Z + 7 days = **2026-10-06T14:13:56Z**:

```
mesh node usage --project runsheet/mesh --from 2026-09-29T14:13:56Z --to 2026-10-06T14:13:56Z --kind build
```

Once the mini starts a release carrying item 10, the after window that isolates it starts at the first context line after that start (the same `grep -anE … | head -1` over the lines after the `mesh node <version>, pid` line), and its rerun is due seven days later.

## The numbers

| Reading | Sessions | Median input tokens | Mean input tokens | Cache read % | Context bytes median | Coverage |
|---|---|---|---|---|---|---|
| before (build) | 138 | 1119187 | 1820329 | 96.5 | - | 138 records of 219 sessions ended in the window |
| before (evaluate) | 131 | 418105 | 523584 | 88.9 | - | 131 records of 213 sessions ended in the window |
| after items 1–4, before item 10 (build) | - | - | - | - | - | 0 records of 38 sessions ended in the window |

The after reading is preliminary — it has 0 build sessions against the 20 it needs — so the number that must move, median input tokens per build session (1,119,187 before), has no after value yet; it is due at 2026-10-06T14:13:56Z. The goal's rule stands: if median input tokens per build session do not fall, the change is reverted rather than kept for tidiness.

## Owed

To runsheet/runsheet:

1. `UsageReportInput.contextBytes` on the gateway, so the bytes the node sends reach the usage record (the node drops the field until it does, `ErrNoContextUsage`).
2. The platform's reading, by the operator: `mesh-bench.py` in runsheet/runsheet, cost per merged item and median input tokens per build session for project `runsheet/mesh` over the seven days before and after the first release carrying items 1–4 and 10.

On this project:

3. The after reading's rerun on 2026-10-06T14:13:56Z, `mesh node usage --project runsheet/mesh --from 2026-09-29T14:13:56Z --to 2026-10-06T14:13:56Z --kind build`, with its coverage — and the window from the mini's first context line on a release carrying item 10, rerun seven days after it.
4. The revert rule: if that median does not fall below 1,119,187, items 1–4 and 10 are reverted.
5. Unmerged per `mesh_items`: item 9 ([mesh-cli#464](https://github.com/runsheet/mesh-cli/pull/464), review) and item 11 ([mesh-cli#466](https://github.com/runsheet/mesh-cli/pull/466), review), which the after reading needs on the mini; item 13 ([mesh-specs#107](https://github.com/runsheet/mesh-specs/pull/107), review); v0.2.194's failed release build, so a published release carries item 10.

## Log

- 2026-09-29 11:24Z — goal set.
- 2026-09-29 12:04Z — item 1 merged, mesh-cli#421.
- 2026-09-29 12:27Z — item 2 merged, mesh-cli#423.
- 2026-09-29 12:59Z — item 4 merged, mesh-cli#429.
- 2026-09-29 13:26Z — item 3 merged, mesh-cli#422.
- 2026-09-29 — item 7 merged, mesh-specs#101; items 5 and 8 failed on the measurement; item 6 cancelled, carried as item 9.
- 2026-09-29 14:07Z — v0.2.191 released; the mini started it at 14:13:19Z and confirmed it at 14:25:31Z.
- 2026-09-29 15:48Z — item 10 merged, mesh-cli#463; tagged v0.2.194, whose release build failed.
- 2026-09-29 16:17Z — item 12, attempt 3: this document, the readings taken with item 11's branch.
