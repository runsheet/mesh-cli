---
title: Goal — A goal's doc is written once, when it finishes, in one repo
kind: plan
status: draft
parent: prd
---

# Goal — A goal's doc is written once, when it finishes, in one repo

Mesh goal `goal-docs-on-finish`, running.

## Statement

The docs pass of 2026-09-23 found every specs/goal-*.md stuck at status draft: mesh-cli writes the goal doc on each item's branch while the goal runs and nothing rewrites it when the goal ends, the same goal is written into every repo it touches (nine duplicated in runsheet/console) and the copies drift, and the writer omits summary:, which AGENTS.md requires. Write it into the goal's primary repo only, and on the goal's end write the final state (status approved when done, a summary line with items merged and spend).

## Acceptance

mesh-cli tests: a goal spanning two repos writes one doc; a finished goal's doc carries status approved and a summary; items' branches no longer carry the doc table churn. The goal-doc renderer (internal/node/goaldoc.go) is the only writer.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | goaldoc.go: a summary: line, always, and a finished goal's final state | merged | mesh/goal-docs-on-finish-1 | https://github.com/projectmesh-io/mesh-cli/pull/12 | $1.38 |
| 2 | runner.go: write the doc once, when the item that finishes the goal pushes | failed | — | — | $0.00 |
| 3 | runner.go: stop writing the goal doc on every item's own branch | running | — | — | $0.47 |
| 4 | runner.go: write the goal's doc once, into its primary repo, when the goal ends | pending | — | — | $0.00 |
| 5 | trd.md: document the goal-doc process — written once, in the primary repo, at the goal's end | evaluating | mesh/goal-docs-on-finish-5 | — | $0.18 |

## Log

- 2026-09-23 · item 1 · session started
- 2026-09-23 · item 1 · Done. **What changed** - `internal/node/goaldoc.go`: - `GoalDoc.SpentCents` — the goal's total spend taken from the platform's budget-guard number, not summed over `Items`. - `Summary(d GoalDoc) string` — for `done`/`cancelled`, `"%d of %d items merged · $%.2f spent"` (merged counted over `len(d.Items)`, spend from `SpentCents/100`); otherwise the statement's first line collapsed to one line, falling back to the title, and finally to `Mesh goal <slug>` so the line is never empty (needed for the always-non-empty invariant, e.g. a doc with no title and no statement). - `RenderGoalDoc` now emits `summary: <line>` in the frontmatter for every status, and repeats it as `**<line>**` under the `Mesh goal \`slug\`, done.` line only when the goal has ended. - `internal/node/runner.go`: `RecordGoal` passes `goal.SpentCents` into the `GoalDoc`. No other file touches the rendering — `RenderGoalDoc`/`WriteGoalDoc` remain the only writer. - `internal/node/goaldoc_test.go`: extended, not replaced. The existing `TestRenderGoalDoc` frontmatter assertion gained the `summary:` row; three new tests cover (1) a non-empty `summary:` for statuses `""`, `running`, `paused`, `done`, `cancelled`; (2) `done` + three items (two merged) + `SpentCents: 1234` rendering `summary: 2 of 3 items merged · $12.34 spent` in both frontmatter and body; (3) `running` and `""` rendering the statement's first line, falling back to the title, and not carrying the body sentence. **Acceptance** — ran `gofmt -l`, `go vet ./... && go build ./cmd/mesh/ && go test ./...` (the toolchain lives at `/usr/local/go/bin`, not on the default PATH): clean, all packages `ok`, `internal/node` included. No ADR or TRD amendment: this changes no aggregate, policy, process, or public contract — the gateway field `spentCents` already exists and is already fetched.
- 2026-09-24 · item 3 · session started
- 2026-09-24 · item 3 · Poll-mode builds no longer write the goal doc on their own branch. The acceptance check passes: `go vet ./... && go build ./cmd/mesh/ && go test ./...` is clean, and gofmt too. Everything is in one commit, `c58021f`. **What changed** - **`internal/node/runner.go`:** I removed both `r.record(...)` calls from `RunItem`, one at "session started" and one after the session finished. I also deleted `record` and `RecordGoal`. Grepping the file for `record`, `GoalDoc`, `WriteGoalDoc`, `specs/goal` or `Commit(` now finds nothing. `goaldoc.go` is untouched. - **`internal/node/loop_test.go`:** a new helper, `assertNoGoalDoc`, fails the test if there is any commit containing `docs(mesh): goal`, or any `specs/goal-*.md` file in a workspace the runner prepared. It replaces the old checks in `TestLoopClaimsRunsAndFinishes` (two commits expected, doc read from the item's branch). The other three RunItem tests (`TestRunnerFailsAnUnsuccessfulSession`, `TestRunnerLeavesItemOnShutdown`, `TestRunnerAsksForItsMCPServersPerItem`) had no doc assumptions, but they now call the helper too. - **`README.md`:** the goal-record section said poll mode writes the doc for the session. It now says poll mode no longer writes it on an item's branch and the doc is written once, when the goal ends. **One thing outside the brief:** channel mode (`internal/channel/tools.go`, the `mesh_record` tool) also used `node.RecordGoal`. So that deleting it wouldn't break channel mode, I moved the same write-and-commit into `tools.go`, calling `goaldoc.go`'s `WriteGoalDoc` directly. Channel mode works exactly as before, which means it still writes the doc on the item's branch whenever the session calls `mesh_record`. The goal's acceptance ("items' branches no longer carry the doc table churn") covers that path too, so a later item needs to handle it: item 4, or one of its own. The public contract hasn't changed (no gateway fields, no reports or claims), so I didn't amend any ADR or TRD. Item 5 is the TRD write-up.
