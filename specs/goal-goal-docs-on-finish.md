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
| 1 | goaldoc.go: a summary: line, always, and a finished goal's final state | claimed | — | — | $0.00 |
| 2 | runner.go: write the doc once, when the item that finishes the goal pushes | pending | — | — | $0.00 |

## Log

- 2026-09-23 · item 1 · session started
