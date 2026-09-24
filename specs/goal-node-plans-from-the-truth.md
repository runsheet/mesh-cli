---
title: Goal — The node plans and builds from what main and the platform hold
kind: plan
status: draft
parent: prd
---

# Goal — The node plans and builds from what main and the platform hold

Mesh goal `node-plans-from-the-truth`, running.

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
| 16 | INDEX.md lists the project's live goals and items from the platform | pending | — | — | $0.00 |
| 17 | Gateway.FinishWorkItem and WorkItem carry already_done with evidence | pending | — | — | $0.00 |
| 18 | The builder checks main first and can hand back already done | pending | — | — | $0.00 |
| 19 | The planner's replan checks main first | pending | — | — | $0.00 |
| 20 | The evaluator confirms an already-done claim | pending | — | — | $0.00 |
| 21 | mesh_record and RunItem stop writing status tables into git | running | mesh/node-plans-from-the-truth-21 | — | $1.93 |
| 22 | Replay test: a rebuilt-item scenario ends already done under $1 | pending | — | — | $0.00 |

## Log

- 2026-09-24 · item 21 · The branch is now back to `origin/main` plus the two real commits. The stray goal doc is gone from the branch, but the node may add it back when it pushes, because the binary it's running is older than `main`. **What I did** - I reset the branch to `93d3904`. That dropped `c2bc390` and a second stray commit, `2a6cabd`. Both were `docs(mesh): goal node-plans-from-the-truth — item 21` commits that added or updated `specs/goal-node-plans-from-the-truth.md`. - The branch now holds only `73e8f95` (the `mesh_record` fix) and `93d3904` (the README). Both sit directly on `origin/main` (`3984c54`), so no rebase was needed. - I didn't call `mesh_record` in this session and changed no code. The reviewer passed everything else last time. **Where the doc commits come from.** Neither this branch nor `main` writes them. `main`'s `RunItem` stopped writing the goal doc in #38 (`8b4395f`), and `grep -rn WriteGoalDoc` finds only the function itself in `goaldoc.go` (plus its own test). So the commits were added after the session by a node binary that predates #38, or by an old binary whose channel `mesh_record` still commits the table. I can't deploy from inside this workspace or force-push (rule 5). **Deploy a binary built from current `main` to the node before it pushes this branch,** or the old code will add the doc commit again. The push has to replace the old remote branch, which still has `c2bc390`/`2a6cabd`, so it needs `--force-with-lease`. **Acceptance check** - `go vet ./...`, `go build ./cmd/mesh/` and `go test ./...` all pass with no failures. - `git diff --name-only origin/main...HEAD` lists only the README, `internal/channel/{channel_test,session,tools}.go` and `internal/node/runner_test.go`. No `specs/goal-*` file. - The runner test `TestRunItemBranchCarriesNoGoalDoc` and the channel test `TestToolsAgainstGateway` are in the suite: the first checks the item branch carries no `specs/goal-*` file, the second that `mesh_record` makes no commit and passes its summary to the gateway. - The README and the channel prompt (`internal/channel/session.go:98`) describe `mesh_record` as a platform-only record that writes nothing into git. **For the PR** - ADR-003 decision 4 and TRD §3 in mesh-specs already say no item branch writes a goal status table, so neither was changed. The PR should cite them. - `WriteGoalDoc` is left untouched, as the brief asks, because goal-docs-on-finish's finish-time writer isn't on `main` yet.
