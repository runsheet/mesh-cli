---
title: A session's context has a budget that actually holds, and the node reports what it cost
kind: plan
status: draft
parent: prd
summary: node.yaml's context budget now bounds every binding document, INDEX.md's summaries are trimmed and the node reports what its context cost; the before reading's cost figure is taken, no node has run the new binary yet, and median input tokens per build session cannot be measured at all until the platform exposes one.
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

It was not run. The brief's fallback is `mesh_evidence` and `mesh_items` "for build sessions" — before trusting that, this attempt went looking for anything closer to an actual per-build-session ledger, since neither tool documents a token field and a prior attempt was rejected for treating an item-level total as if it were session-level:

- **This node's own local session records**, which is where a build's raw token counts actually live while they exist. `~/.mesh/work/<goal>/<seq>.session/output.jsonl` carries every turn's `usage` block (`input_tokens`, `cache_read_input_tokens`, …) for a **running** build — this item's own `5.session` has one — but `harness.RemoveSession` deletes that directory once the session ends (confirmed: `1.session`, `2.session`, `3.session`, `4.session` under this goal's work root are gone; only the checkouts and `*.attestation.json` remain). A finished build's tokens are not recoverable from disk after the fact, only while it is still running.
- **`~/.mesh/work/kept-sessions/`**, the 24-hour output retention the brief's own context describes. All 403 directories there carry `"kind": "evaluate"` in `state.json` — this retention is Codex's (`KeepSessionOutput`, evaluate sessions only); no build session's output lands here at all, so even the last 24 hours of build tokens are unreachable this way.
- **`~/.mesh/node.log`** (354MB, this node's full run log): the harness only logs a local `usage: {…}` line as a *fallback* when the platform's finishing command refuses the usage report; grepping for an actual runtime line (`^mesh node: <timestamp> … usage: {`) returns zero matches — every session's usage lands on the platform normally, so the local fallback never fires and node.log carries no token data. (Every naive match for `usage:` or `context … bytes` in this file is inside a session's own diff output — code or prose the session was reading or writing — not a real log line; a timestamp-anchored grep confirms it.)
- **`~/.mesh/ledger.json`**, the local spend ledger: per-account dollars for the day and month only (`dayUSD`, `monthUSD`), no per-session or per-project breakdown, no tokens.
- **The merged pull requests themselves** (`gh pr view 421 --repo runsheet/mesh-cli --json body,comments`, and the same for #423, #429): the evaluator's notes are in the body; no comment carries a cost or token summary.

None of these carries a token count for a finished build session, before or after. Falling back the rest of the way to `mesh_evidence` and `mesh_items`, as the brief allows when the script cannot be found or run:

**Source.** `mesh_evidence(project="runsheet/mesh", limit=500)` (no goal filter — every merged item across the whole project, since the change touches every session kind) joined with `mesh_items(project="runsheet/mesh", goal=<slug>)` for each of the 14 goals it names, matched on `(goal_slug, item_seq)`. **Neither tool carries a token field of any kind** — confirmed twice, once against the tools' own field lists and once against a raw record's full JSON (dumped in full for item 4: `approvals`, `checks`, `completeness`, `evaluation` {cost_cents, evaluated_at, evaluator_node_id, notes, passed}, `goal_id`, `goal_slug`, `item_seq`, `item_title`, `merge` {…}, `missing`, `pr_number`, `pr_url`, `project_id`, `recorded_at`, `repo_full_name`, `repo_id`, `work_item_id` — no tokens anywhere). This is the reason the token figure below is missing rather than estimated: it is not a gap in this search, it is a gap in what the platform records.

**A second gap, found by spot-checking rather than assumed:** `mesh_items`' `cost_cents` for a merged item is not a build-session figure either. It exactly equals `mesh_evidence`'s `evaluation.cost_cents` for every merged item checked — items 1, 2 and 4 of this goal, and all four items of goal `tools-show-their-use` (8 for 8, cents-for-cents). A *running* item's `cost_cents` does track its live build (this item, #5, mid-build: `cost_cents` 225 in `mesh_items`, `cost_cents` 225 on the same session in `mesh_health_nodes`), so build cost is tracked somewhere — but once an item merges, `cost_cents` reads as the evaluation's cost alone, not build-plus-evaluation. No tool here separates a merged item's build-session cost from its evaluation cost; "cost per merged item" below is the only number available from `mesh_items`, and it should be read as such, not as an isolated build-session figure.

**Before.**

```
mesh_evidence(project=runsheet/mesh, limit=500): 72 records (unchanged on a
  second pull with a higher limit — this is everything the platform
  retains for the project, not a limit artifact)
window observed: 2026-09-28T05:02:40Z – 2026-09-29T12:59:16Z (32h57m)
  — the brief asks for the seven days ending the day items 1-4 merged
    (2026-09-22 through 2026-09-29); the platform's evidence retention
    for this project does not reach back that far. This is a platform
    limitation (see Owed), not a shorter search: a second pull with
    limit=500 (vs. the default 50) returned the identical 72 records
    and the identical window.
merged items: 72
cost per merged item (evaluation cost only, see above): mean 280.5¢ ($2.81),
  median 218.5¢ ($2.19)
median input tokens per build session: not available at any window —
  no tool or on-disk source reachable from this node carries a token
  count for a finished build session (see the search above).
```

**After: no reading exists yet.** `mesh_health_nodes(project="runsheet/mesh")` at 2026-09-29T13:23:55Z (re-checked, not stale) shows the project's one node, "Mini", at `version: v0.2.188`, `behind: true`, `commits_behind_main: 23` — main's head is still `531ab15` (item 4's own merge commit), so the running binary predates every item of this goal. Confirmed independently: `~/.mesh/node.log` has no runtime line matching `context \d+ bytes in \d+ files` (item 4's log line), so no build has run the new code yet. This matches the acceptance's own case: release blocked, the mini not updated.

Exact steps for the after reading, once it can be taken — **for cost per merged item only**, since that is the only figure any tool here can produce:

1. Get a release past `531ab15` onto the mini — either the automatic path (`release-on-merge.yml` tags on the next node-changing merge to main and the mini follows `desiredVersion`), or the by-hand fallback in this repo's README ("Upgrading a node without ending sessions"): cross-build, `scp` to `~/bin/mesh.new`, `mesh node upgrade --binary ~/bin/mesh.new`.
2. Confirm the new binary actually ran a session: `mesh_health_nodes` shows `behind: false` (or `commits_behind_main` at or below the count for `531ab15`), or grep `~/.mesh/node.log` for a line matching `context \d+ bytes in \d+ files, \d+ documents to fetch` after the upgrade, or check a recent `mesh_evidence` record's usage for `contextBytes` once the platform carries the field (see Owed).
3. Let sessions accumulate for a comparable window to the before reading (the platform's evidence retention, currently ~33 hours for this project — re-check with `limit=500` first, since a longer retention would let this reading use more history than the before one did).
4. Re-run this section's derivation: `mesh_evidence(project="runsheet/mesh", limit=500)` for the after window, mean and median of `evaluation.cost_cents` per merged item.

**For median input tokens per build session, there are no steps to write.** Every source this attempt checked — `mesh_evidence`, `mesh_items`, `mesh_health_nodes`, this node's own session directories, its 24-hour Codex retention, its log, its spend ledger, the merged PRs — carries no token count for a finished build session, and nothing an operator can do on this node changes that: it needs one of the three things in Owed #1 to exist first. Landing a new binary on the mini is necessary for the cost reading above but does nothing for the token reading; that is a separate, still-open gap.

| reading | median input tokens / build session | cost per merged item (evaluation cost only) |
|---|---|---|
| before | not available — no source anywhere on this node or the platform carries it | mean 280.5¢ ($2.81), median 218.5¢ ($2.19), n=72, window 32h57m |
| after | not available, and no procedure exists to make it available (see above) | not taken — no node has run a binary past item 4 |

No number can be said to have moved: the only figure either reading produced is evaluation cost per merged item over a 33-hour window the platform itself limits, not the goal's own metric; there is no after reading of any kind yet; and median input tokens per build session — the number the goal's Acceptance and Statement both turn on — could not be produced for *either* window by any tool or on-disk source available to this node, so it is not a measurement this item deferred, it is one the platform does not yet support. Per the goal's own rule, until that gap closes and an after reading of input tokens exists, the change's effect on the thing it was meant to fix is unverified and must not be treated as validated, let alone kept for tidiness.

## Owed

To runsheet/runsheet:

1. **A way to measure median input tokens per build session at all.** No tool (`mesh_evidence`, `mesh_items`, `mesh_health_nodes`) carries a token count, no on-disk retention covers a finished build (`kept-sessions` is Codex-evaluate-only, 24 hours; a build's own `<seq>.session/output.jsonl` is removed by `harness.RemoveSession` the moment it ends), and `mesh-bench.py` cannot be found anywhere this node can reach. Any one of the following would close it: publish or surface `mesh-bench.py`'s own reading through a tool; add a token count to `mesh_evidence`/`mesh_items`; or retain a finished build's `output.jsonl` for a window the way Codex evaluate sessions already are, so a local script could compute it without a platform change. Without one of these, this goal's own central metric — the thing the Statement and Acceptance both name — cannot be measured before or after, on this node or any other, now or later.
2. A merged item's `cost_cents` cannot be trusted to isolate build-session cost from evaluation cost: it was found, by checking rather than assuming, to equal `mesh_evidence`'s `evaluation.cost_cents` exactly for every merged item sampled (8 of 8 across two goals), even though a running item's `cost_cents` does track its live build. Either expose the two separately, or document plainly that a merged item's `cost_cents` is evaluation cost, not total session cost, so a future reading does not repeat this assumption.
3. `UsageReportInput.contextBytes` (an `Int`, stored on the usage record) — item 4's own ADR-016 already names this; until it lands every report is sent once with the field, refused, and re-sent without it.
4. The after reading itself, once a node has run a binary past `531ab15` — see Measurement's exact steps (cost per merged item only; #1 above is the prerequisite for the token half).
5. Item 3 ([mesh-cli#422](https://github.com/runsheet/mesh-cli/pull/422)) is still in review, not merged; this doc's Items section and the ADR-015 amendment describe it as it stands on this item's branch, not as merged history.
