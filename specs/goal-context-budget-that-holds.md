---
title: Goal — A session's context has a budget that actually holds, and the node reports what it cost
kind: plan
status: draft
parent: prd
summary: The node's context budget is configurable in node.yaml, binding documents count against the total, INDEX.md is trimmed and inside the budget, each session logs its context bytes; the node-side before reading is taken, and the after reading is owed until the mini runs v0.2.194 or later.
---

# Goal — A session's context has a budget that actually holds, and the node reports what it cost

Mesh goal `context-budget-that-holds`, running.

## Statement

On 2026-09-28 Mesh ran 500 sessions: 1.16 billion input tokens against 7.3 million output, with 97% of the input from cache reads. Nearly all of it was the same context, read again on every turn. The budget in `internal/node/workspace.go` did not hold. `ContextBytesMax` and `DocBytesMax` were compile-time constants. Instructions and ADRs were exempt from the total. `INDEX.md` listed every document with its summary. The goal makes the budget configurable (node.yaml `context:`), makes binding documents count against the total (the item's own repos first, never truncated), trims the index and puts it inside the budget, and logs what each session's context cost. It also takes readings before and after: if input tokens per build session do not fall, the change is to be reverted.

## Acceptance

`go vet ./...` clean, `gofmt -l .` empty, both builds pass, `go test ./...` green. `context: {bytes_max, doc_bytes_max}` loads from node.yaml, `Reload` re-reads it, and today's 240KB and 60KB apply when it is absent. A test proves the total holds when there are more binding documents than fit, the item's own repo wins, and nothing is truncated. `INDEX.md`'s trimmed rows are asserted as exact lines. One log line per session records the context bytes and the documents left to fetch, and the bytes ride the usage record. This document holds the reading before and after, with the median input tokens per build session in both.

## Items

Read from `mesh_items` (project runsheet/mesh, goal context-budget-that-holds) at 2026-09-29T17:06:06Z; item 16 is written as `building`.

| Seq | Title | Status | PR |
| --- | --- | --- | --- |
| 1 | Node: `context:` in node.yaml — bytes_max and doc_bytes_max, read again each tick like the accounts, defaults 240KB and 60KB; FetchContext takes the budget instead of constants | merged | https://github.com/runsheet/mesh-cli/pull/421 |
| 2 | Node: instructions and ADRs count against the total — the item's own repos first, then the rest by recency; what does not fit is in the index, never truncated; ADR-015 | merged | https://github.com/runsheet/mesh-cli/pull/423 |
| 3 | Node: INDEX.md keeps one row per document but carries a summary only for the kinds a session fetches and that are not on disk | merged | https://github.com/runsheet/mesh-cli/pull/422 |
| 4 | Node: one log line per session with the context bytes written and the documents left to fetch, and `contextBytes` on the usage record with the older-platform fallback; ADR-016 | merged | https://github.com/runsheet/mesh-cli/pull/429 |
| 5 | Goal doc: context-budget-that-holds — the reading before and after on the same project, the number that moved, what landed and what is owed; ADR-015 records the index trim and the reporting | failed | — |
| 6 | Node: every session's usage record is logged on its label with its kind, whatever became of the report, and `mesh node usage` reads node.log into the reading — sessions, median and mean input tokens, cache-read share and median context bytes by kind, for a window; ADR-016 amended | cancelled | https://github.com/runsheet/mesh-cli/pull/448 |
| 7 | mesh-specs: the TRD's context-on-disk paragraphs and adr-003 name the context budget — node.yaml `context:`, the binding order, never truncated, the trimmed index, the per-session log line and `contextBytes` owed on UsageReportInput | merged | https://github.com/runsheet/mesh-specs/pull/101 |
| 8 | Goal doc: context-budget-that-holds — the node's reading before and after from `mesh node usage` on the same project with its coverage stated, the platform's mesh-bench.py reading named as the operator's and owed, what landed and what is owed | failed | — |
| 9 | Node: item 6's work on main — every session's usage record logged on its label with its kind, and `mesh node usage` reads node.log into the reading (PR #448 carried) | merged | https://github.com/runsheet/mesh-cli/pull/464 |
| 10 | Node: INDEX.md is inside the budget — its rows are reserved from bytes_max before documents are placed, the kinds a session never reads are listed in a compact second table, and the bytes on disk never exceed bytes_max; ADR-015 amended | merged | https://github.com/runsheet/mesh-cli/pull/463 |
| 11 | Node: `mesh node usage` reads a precise window and one project — `--from`/`--to` take RFC 3339 timestamps as well as dates, `--project org/slug` narrows to the project's goals resolved from the platform (or `--goals <file>` offline), and the reading prints its coverage: finished sessions of each kind in the window beside the records read; ADR-016 amended | cancelled | https://github.com/runsheet/mesh-cli/pull/466 |
| 12 | Goal doc: context-budget-that-holds — the node's before and after readings from `mesh node usage --project runsheet/mesh` with timestamps and coverage, the after window starting at the first context line of the release carrying items 1–4 and 10, the platform's mesh-bench.py reading named as the operator's and owed, every item as mesh_items records it, and what is owed | failed | — |
| 13 | mesh-specs: the TRD's context budget paragraphs say INDEX.md is inside bytes_max, reserved first, in two tables with the live section outside; adr-003 decision 3 amended in a dated line | merged | https://github.com/runsheet/mesh-specs/pull/107 |
| 14 | Node: item 11's work on main — `mesh node usage` takes RFC 3339 `--from`/`--to`, `--project org/slug` or `--goals <file>`, and prints its coverage lines; ADR-016 amended (PR #466 carried) | cancelled | https://github.com/runsheet/mesh-cli/pull/472 |
| 15 | Goal doc: context-budget-that-holds — the reading before and after from a main-built `mesh node usage --goals` on the mini's node.log, the median input tokens per build session that moved, every item as mesh_items records it, and what is owed | failed | — |
| 16 | Goal doc: context-budget-that-holds — `mesh node usage --goals` readings before and after from a binary at this branch's head (PR #472 carried if main lacks it), the median input tokens per build session named, every item as mesh_items records it, mesh-bench.py owed to the operator | building | — |

## Measurement

Binary: `go build -o /tmp/mesh ./cmd/mesh/` at commit `ed07f9cf7e23a4a59f04d5fc24a310af97da8f53`, this branch's own head when the binary was built. It is **main plus PR #472**: `origin/main` (`bae3229`) has no `--goals` flag in `internal/commands/nodeusage.go`, so that commit is `origin/main` with PR #472's eight files taken unchanged from `origin/mesh/context-budget-that-holds-14`. The only commit after it on this branch adds this document and changes no code, so a binary built from the branch's final head is the same program.

Where the after window starts. Item 10 (PR #463, commit `10a9ba2`) is first in release v0.2.194 (`git tag --contains 10a9ba2`: v0.2.194, v0.2.195, v0.2.196, …; the latest release is v0.2.199). The mini's node.log has no start line from v0.2.194 or later: its last is `mesh node: 2026/09/29 14:25:00 mesh node v0.2.191, pid 794933, config /home/chris/.mesh/node.yaml`, and the node was still logging on v0.2.191 at 17:27 (its last line, 2026/09/29 17:27:02). **No node has yet written a context line from a release that carries items 1–4 and 10**, so that after reading cannot be taken on this date. The after window below therefore starts at the first context line any node wrote (v0.2.191, which carries items 1–4). node.log's timestamps are local, and this host's zone is +0000:

```
mesh node: 2026/09/29 14:13:56 item evaluation-reports-no-verdict-platform/11: context 0 bytes in 0 files, 0 documents to fetch
```

The before window is the seven days ending at 2026-09-29T14:13:56Z. The after window runs from that moment to when it was read. Both commands ran with `/tmp/mesh` on this host's `~/.mesh/node.log`, with the goals file below saved as `runsheet-mesh-goals.txt`:

```
mesh node usage --from 2026-09-22T14:13:56Z --to 2026-09-29T14:13:56Z --goals runsheet-mesh-goals.txt
window 2026-09-22T14:13:56Z..2026-09-29T14:13:56Z  records 2026-09-28T11:41:15..2026-09-29T14:13:09
kind      sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes
build     138       1119187              1820329            96.5          -                     0
evaluate  131       418105               523584             88.9          -                     0
rebase    12        612716               2375922            97.5          -                     0
coverage build: 138 records of 454 sessions ended in the window
coverage evaluate: 131 records of 410 sessions ended in the window
coverage rebase: 12 records of 19 sessions ended in the window
coverage plan: 0 records of 84 sessions ended in the window
unparsed 0
```

```
mesh node usage --from 2026-09-29T14:13:56Z --to 2026-09-29T17:18:41Z --goals runsheet-mesh-goals.txt
window 2026-09-29T14:13:56Z..2026-09-29T17:18:41Z  records -
kind  sessions  input tokens median  input tokens mean  cache read %  context bytes median  with context bytes
coverage build: 0 records of 59 sessions ended in the window
coverage evaluate: 0 records of 55 sessions ended in the window
coverage rebase: 0 records of 5 sessions ended in the window
coverage plan: 0 records of 9 sessions ended in the window
unparsed 0
```

The goals file: every goal of project runsheet/mesh as `mesh_goals` listed it at 2026-09-29T17:06Z, one slug a line, sorted:

```
acceptance-the-evaluator-can-see
a-finished-session-leaves-no-process
a-node-serves-the-orgs-that-enrol-it
builtin-is-mesh-context
bursting-across-accounts
codex-adapter
codex-provider-for-any-openai-compatible-model
codex-sessions-reach-the-context-tool
context-budget-that-holds
directions-on-the-node
evaluation-reports-no-verdict
evaluation-sees-the-merge-base
evaluations-run-on-codex
finish-before-you-start
goal-docs-on-finish
loop-schedules-on-issued-accounts
mesh-cli-crypto-upgrade
mesh-cli-lives-in-runsheet
mesh-spends-less-per-item
node-attests-its-validation
node-builds-tool-config
node-claims-by-priority
node-client-fields-are-generated
node-drains
node-out-of-credit
node-plans-from-the-truth
node-reads-its-limits
node-releases-on-merge
node-reports-usage-on-the-finish
node-runs-on-many-accounts
node-runs-rebase-sessions
node-runs-the-items-model
node-sandboxes-sessions
node-says-why-work-waits
node-scales-with-headroom
node-sees-every-linked-accounts-repos
node-self-update
node-updates-from-a-private-repo
node-upgrades-keep-sessions
node-uses-item-tokens
node-yaml-takes-effect-or-says-why-not
pitching-needs-no-room
plans-depend-only-on-what-they-use
public-releases-on-the-mesh-repo
release-tag-mirrors-with-a-rigger-binding
research-pitches
sandboxed-sessions
signed-releases-on-runsheet-mesh
the-mirror-publishes-without-a-stored-token
the-node-serves-its-enterprise
the-node-serves-its-owners-orgs
token-fetch-retries
tools-show-their-use
upgrades-survive-a-slow-platform
```

Median input tokens per build session: before 1119187, after none (the after output has no build row: 0 records of 59 build sessions)

Whether the number fell cannot be said yet: v0.2.191 logs each session's usage without the `<label>: usage <kind>` label that item 9 (PR #464) adds, so `mesh node usage` finds no records to give a kind in the after window, and the revert test in the Statement cannot be applied. The after reading is thin — it holds no build sessions at all, fewer than 20 — and becomes a reading only once the mini runs v0.2.194 or later: the window to read is the seven days from the first `context <n> bytes in <m> files` line that release writes, and a window started today would reach seven days on 2026-10-06. The before reading is partial too: its records start at 2026-09-28T11:41:15 and cover 138 of the 454 build sessions that ended in the window.

**The operator's reading.** The Statement asks for a `mesh-bench.py` reading. That is the platform's reading, not the node's, and it is owed to the operator. The platform can serve it: `Query.usageRecord` exists in `internal/gateway/schema.json`. But this session has no console credential to read it. The platform also does not yet record `contextBytes` on `UsageReportInput`: the field is not in `internal/gateway/schema.json`, and the node drops it (`ErrNoContextUsage`). That field is owed on the platform side before `mesh-bench.py` can show context bytes beside input tokens.

## What this project did not build

- The platform's `mesh-bench.py` reading, before and after: owed to the operator (see The operator's reading).
- `contextBytes` on the platform's `UsageReportInput`: owed in runsheet/runsheet. Until it lands, the node logs the bytes and drops them from the report.
- Item 5 failed: its before median came from partial log samples (only pushed builds with a nearby `usage:` line, records from September 25 inside a window starting September 23), and it had no after reading.
- Item 8 failed: its after window started at midnight on September 29, before the first context line at 14:13, so all 90 of its build records predated the change. It also gave the wrong seven-day date, and gave no command to rebuild its goals file.
- Item 12 failed: its readings came from item 11's unmerged branch, and a main-built binary ran its quoted command only to fail with `unknown flag: --project`.
- Item 15 failed: its branch added no goal document, and `git diff --stat origin/main` listed 24 files, including Go files, because it was based on an out-of-date branch.

## Log

- 2026-09-29T12:04:26Z · mesh-cli #421 merged (item 1, node.yaml `context:`)
- 2026-09-29T12:27:38Z · mesh-cli #423 merged (item 2, binding documents count against the total, ADR-015)
- 2026-09-29T12:59:15Z · mesh-cli #429 merged (item 4, the per-session context line and `contextBytes`, ADR-016)
- 2026-09-29T13:26:51Z · mesh-cli #422 merged (item 3, the trimmed index)
- 2026-09-29T15:48:41Z · mesh-cli #463 merged (item 10, INDEX.md inside the budget)
- 2026-09-29T16:24:05Z · mesh-cli #464 merged (item 9, labelled usage records and `mesh node usage`)
- 2026-09-29 · mesh-specs #101 merged (item 7) and #107 merged (item 13); their merge times could not be read from this session
- 2026-09-29T17:18Z · item 16 · readings taken on a main-plus-#472 binary; the mini still runs v0.2.191
