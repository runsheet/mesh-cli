---
title: Goal — The node plans and builds from what main and the platform hold
kind: plan
status: draft
parent: prd
---

# Goal — The node plans and builds from what main and the platform hold

Mesh goal `node-plans-from-the-truth`, awaiting_approval.

## Statement

Rank 6 of runsheet/runsheet specs/proposal-prologue-mesh-next.md, node half (the platform half is goal prologue-live-state on the runsheet project). (1) The node's .mesh/context/INDEX.md lists the project's live goals and items from the platform. (2) mesh_record stops writing status tables into git — coordinate with goal goal-docs-on-finish, which writes one final doc on finish; the two must not both write. (3) The planner's replan and the builder check main first — search recent merged PRs and commits for the item's work — and may hand back 'already done' with evidence, confirmed by the evaluator.

## Acceptance

Tests: INDEX.md carries live item status; no item branch writes a goal status table; replaying the stalled goal's item 7 (which rebuilt item 3's merged work) ends as already done for under $1.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | TRD: an already_done outcome for a WorkItem | failed | — | — | $0.00 |
| 2 | INDEX.md lists the project's live goals and items | failed | — | — | $0.00 |
| 3 | mesh_record and RunItem stop writing status tables into git | failed | — | — | $0.00 |
| 4 | Gateway.FinishWorkItem carries already_done | cancelled | — | — | $0.00 |
| 5 | The builder checks main first and can hand back already done | cancelled | — | — | $0.00 |
| 6 | The planner's replan checks main first | cancelled | — | — | $0.00 |
| 7 | The evaluator confirms an already-done claim | cancelled | — | — | $0.00 |
| 8 | ADR-003 and TRD: a work item can finish already done, and INDEX.md carries live state | failed | — | — | $0.00 |
| 9 | INDEX.md lists the project's live goals and items | cancelled | — | — | $0.00 |
| 10 | Gateway.FinishWorkItem and WorkItem carry already_done | cancelled | — | — | $0.00 |
| 11 | The builder checks main first and can hand back already done | cancelled | — | — | $0.00 |
| 12 | The planner's replan checks main first | cancelled | — | — | $0.00 |
| 13 | The evaluator confirms an already-done claim | cancelled | — | — | $0.00 |
| 14 | mesh_record and RunItem stop writing status tables into git | cancelled | — | — | $0.00 |
| 15 | ADR-003 and TRD: a work item can finish already done; INDEX.md carries live state; git holds no status table | merged | mesh/node-plans-from-the-truth-15 | https://github.com/projectmesh-io/mesh-specs/pull/15 | $0.73 |
| 16 | INDEX.md lists the project's live goals and items from the platform | cancelled | — | — | $0.00 |
| 17 | Gateway.FinishWorkItem and WorkItem carry already_done with evidence | cancelled | — | — | $0.00 |
| 18 | The builder checks main first and can hand back already done | cancelled | — | — | $0.00 |
| 19 | The planner's replan checks main first | cancelled | — | — | $0.00 |
| 20 | The evaluator confirms an already-done claim | cancelled | — | — | $0.00 |
| 21 | mesh_record and RunItem stop writing status tables into git | failed | mesh/node-plans-from-the-truth-21 | — | $2.19 |
| 22 | Replay test: a rebuilt-item scenario ends already done under $1 | cancelled | — | — | $0.00 |
| 23 | Gateway.FinishWorkItem and WorkItem carry outcome and evidence (already_done) | pending | — | — | $0.00 |
| 24 | mesh_record reports to the platform and no longer writes to git | evaluating | mesh/node-plans-from-the-truth-24 | — | $0.90 |
| 25 | INDEX.md lists the project's live goals and items from the platform | pending | — | — | $0.00 |
| 26 | The builder checks main first and can hand back already done | pending | — | — | $0.00 |
| 27 | The planner's replan checks main first | running | — | — | $0.74 |
| 28 | The evaluator confirms an already-done claim | pending | — | — | $0.00 |
| 29 | Replay test: a rebuilt-item scenario ends already done under $1 | pending | — | — | $0.00 |

## Log

- 2026-09-24 · item 27 · session started
- 2026-09-24 · item 27 · Item 27 is done: a replan now checks what main holds before it writes a graph. `go vet ./... && go build ./cmd/mesh/ && go test ./...` passes, and the work is committed on `mesh/node-plans-from-the-truth-27` as `b2278f6`. **What changed** - **Merged items and their PRs** (`internal/node/plan.go`): the planner now takes each item's PR URL and branch from `GoalWorkItems` and passes them into the prompt. - **The planning prompt** (`internal/harness/prompt.go`): - "The items so far" now starts with "Your plan replaces only the live items; merged items stay." Each item shows its PR URL. - A new "What main already holds" section lists the goal's merged items with their PRs. - A new "Check main first" section tells the session to search each repo's recent merged PRs (`gh pr list --repo <owner/name> --state merged`, plus the goal's `mesh/<slug>-` branches) and the default branch's commits (`git log`) since the goal was set. Work main already has is left out. If the planner can't tell whether main already meets an item's acceptance, it keeps the item and says in its brief that an already_done check is expected. - The plan JSON schema is unchanged. - **History in the checkout** (`internal/node/workspace.go`): the read-only clone used to fetch one commit. It now fetches the default branch's last 300 commits (`FetchDepth`), so `git log` has history to read. The director's research sessions use the same fetch, so they get the deeper history too. **Tests** - `TestPlannerReplans` now checks that the prompt contains the merged item with its PR URL, the "What main already holds" section, the search instructions, the leave-out and already_done-check rule, and "replaces only the live items; merged items stay". It also checks that a failed item is not listed as on main. - New `TestFetchCarriesBaseRefHistory` starts from a one-commit-deep clone, adds three commits upstream, and checks that after `Fetch` the checkout has all four commits in order. - `TestParsePlan` and the other plan tests were not changed and still pass. **Limits** - The goal record has no creation date, so the prompt can't give "since the goal was set" as a date. The session has to work it out from the goal's merged PRs and `mesh/<slug>-` branches. - The fresh-clone path runs through `gh repo clone`, which the test can't use offline, so only the refresh path is tested. Both paths use the same depth. I didn't amend the ADR or TRD: this matches ADR-003 decision 2, and no design document mentions the clone depth.
