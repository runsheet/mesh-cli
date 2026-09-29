---
title: A session's context has a budget that actually holds, and the node reports what it cost
kind: plan
status: draft
parent: prd
summary: node.yaml's context budget now bounds every binding document, INDEX.md's summaries are trimmed and the node reports what its context cost; the before reading is taken, no node has run the new binary yet, and the after reading is owed.
---

# A session's context has a budget that actually holds, and the node reports what it cost

Mesh goal `context-budget-that-holds`, running.

## Statement

A day of Mesh on 2026-09-28: 500 sessions, 1.16 billion input tokens against 7.3 million output — 160 to 1, with 97% of the input cache reads. The plan windows are the real currency on a subscription, and one account is already at 83% of its seven-day window with work left unclaimed for lack of headroom. Almost none of that is the model thinking. It is the same context, re-read every turn of every session.

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

Statuses as `mesh_items` records them (project `runsheet/mesh`, goal `context-budget-that-holds`) at 2026-09-29T13:05Z.

- **Item 1** (merged): Node: `context:` in node.yaml — `bytes_max` and `doc_bytes_max`, read again each tick like the accounts, defaults 240KB and 60KB; `FetchContext` takes the budget instead of constants — [mesh-cli#421](https://github.com/runsheet/mesh-cli/pull/421).
- **Item 2** (merged): Node: instructions and ADRs count against the total — the item's own repos first, then the rest by recency; what does not fit is in the index, never truncated; ADR-015 — [mesh-cli#423](https://github.com/runsheet/mesh-cli/pull/423).
- **Item 3** (review, not yet merged): Node: `INDEX.md` keeps one row per document but carries a summary only for the kinds a session fetches and that are not on disk — [mesh-cli#422](https://github.com/runsheet/mesh-cli/pull/422) (open). Its commits (`378cc5a`, `12092c3`) are already in this item's branch history, carried forward from the item's own checkout.
- **Item 4** (merged): Node: one log line per session with the context bytes written and the documents left to fetch, and `contextBytes` on the usage record with the older-platform fallback; ADR-016 — [mesh-cli#429](https://github.com/runsheet/mesh-cli/pull/429).
- **Item 5** (running): this item — the goal doc and the ADR-015 amendment.

## Measurement

**`mesh-bench.py` could not be found.** It is confirmed absent from mesh-cli, mesh-portal and mesh-specs (it is not tracked in any of their repositories). Searched and not found:

- `mesh_search` for `mesh-bench.py` and related terms (`cost per merged item`, `median input tokens per session`, `benchmark`), unrestricted across the org: the only hits were ADR-016 and adr-034 *referring* to `mesh-bench.py`, never the script itself.
- Every `runsheet/runsheet` checkout this node holds under `~/.mesh/work/*/` (dozens of prior items' workspaces) — `git ls-files | grep -i bench` in each, and an on-disk `find -iname '*bench*'` — found nothing; `runsheet/runsheet`'s own `scripts/` directory (`scripts/mini/`, `scripts/resolve-conflict.py`) has no benchmark script.
- `/home/chris/projects/runsheet` (the operator's own checkout on this machine) holds only `gha-deploy`, not the platform.
- The operator's notes: `~/.mesh/` (nodes.yaml, logs, the ledger, the proxy scripts) and the home directory broadly (`~/Kolenko`, `~/bin`, `~/archive`) — no `mesh-bench*` file anywhere on this machine.

It was not run. Falling back to the usage ledger, as the brief allows:

**Source.** `mesh_evidence(project="runsheet/mesh")` (no goal filter — every merged item across the whole project, since the change touches every session kind) joined with `mesh_items(project="runsheet/mesh", goal=<slug>)` for each of the 14 goals with a merge in the evidence window, matched on `(goal_slug, item_seq)`. `mesh_evidence` gives merge time and `evaluation.cost_cents` (evaluation cost only); `mesh_items` gives `cost_cents` per item, the total cost to land it across every session that touched it — the figure "cost per merged item" means. **Neither tool exposes a token count of any kind**, only cost in cents; this is the reason the second figure below is missing rather than estimated.

**Before.**

```
mesh_evidence(project=runsheet/mesh): 72 records, all matched to mesh_items cost_cents
window observed: 2026-09-28T05:02:40Z – 2026-09-29T12:59:16Z (32h57m)
  — this is everything mesh_evidence currently retains for the project;
    it does not reach back to the full seven days the brief asks for
    (2026-09-22 through 2026-09-29). This is the earliest data available.
merged items: 72
cost per merged item: mean 280.5¢ ($2.81), median 218.5¢ ($2.19)
median input tokens per build session: not available —
  mesh_evidence exposes only evaluation.cost_cents; mesh_items exposes only
  item-level cost_cents. No field on either carries a token count, and no
  other tool on this node reports per-session input tokens.
```

**After: no reading exists yet.** `mesh_health_nodes(project="runsheet/mesh")` at 2026-09-29T13:05:21Z shows the project's one node, "Mini", at `version: v0.2.188`, `behind: true`, `commits_behind_main: 23` — main's head is `531ab15` (item 4's own merge commit), so the running binary predates every item of this goal, items 1, 2 and 4 included. No session has logged the item-4 line (`context … bytes in … files, … documents to fetch`) and no usage record has carried `contextBytes`, because no build has run the new code. This matches the acceptance's own case: release blocked, the mini not updated.

Exact steps for the after reading, once it can be taken:

1. Get a release past `531ab15` onto the mini — either the automatic path (`release-on-merge.yml` tags on the next node-changing merge to main and the mini follows `desiredVersion`), or the by-hand fallback in this repo's README ("Upgrading a node without ending sessions"): cross-build, `scp` to `~/bin/mesh.new`, `mesh node upgrade --binary ~/bin/mesh.new`.
2. Confirm the new binary actually ran a session: `mesh_health_nodes` shows `behind: false` (or `commits_behind_main` at or below the count for `531ab15`), or grep `~/.mesh/node.log` for a line matching `context \d+ bytes in \d+ files, \d+ documents to fetch` after the upgrade, or check a recent `mesh_evidence` record's usage for `contextBytes` once the platform carries the field (see Owed).
3. Let build sessions accumulate for a comparable window to the before reading (roughly 33 hours, or seven days if the platform's evidence retention has grown by then).
4. Re-run this section's derivation: `mesh_evidence(project="runsheet/mesh")` for the after window, joined with `mesh_items` per goal for `cost_cents`, mean and median cents per merged item.
5. For median input tokens per build session, this reading is blocked on the same gap noted above: either `mesh-bench.py` needs to be found (or published somewhere this node can reach) or the platform needs to expose token counts through `mesh_evidence`/`mesh_items` — see Owed.

| reading | median input tokens / build session | cost per merged item |
|---|---|---|
| before | not available (no tool on this node exposes it) | mean 280.5¢ ($2.81), median 218.5¢ ($2.19), n=72 |
| after | not taken — no node has run a binary past item 4 | not taken |

No number has moved, because there is no after reading to compare against and no tool available to this node ever produced an input-token figure for the before reading either; per the goal's own rule, if the after reading — once it exists — shows median input tokens per build session did not fall, the change should be reverted rather than kept.

## Owed

To runsheet/runsheet:

1. `UsageReportInput.contextBytes` (an `Int`, stored on the usage record) — item 4's own ADR-016 already names this; until it lands every report is sent once with the field, refused, and re-sent without it.
2. A token count on `mesh_evidence` or `mesh_items` (or `mesh-bench.py`'s own reading surfaced through a tool), so the goal's central measurement — median input tokens per build session — can be taken by a session on this node at all. Without it, no future goal's before/after reading is any easier than this one's.
3. The after reading itself, once a node has run a binary past `531ab15` — see Measurement's exact steps.
4. Item 3 ([mesh-cli#422](https://github.com/runsheet/mesh-cli/pull/422)) is still in review, not merged; this doc's Items section and the ADR-015 amendment describe it as it stands on this item's branch, not as merged history.
