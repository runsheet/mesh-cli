---
title: Goal — The node claims urgent work first
kind: plan
status: draft
parent: prd
---

# Goal — The node claims urgent work first

Mesh goal `node-claims-by-priority`, running.

## Statement

Goals now carry a priority (urgent, high, normal, low; empty reads as normal) — runsheet's merge-queue goal added GoalIndex.priority, and the integrator merges each repo's PRs in that order. But the node still claims ready work oldest seq first (internal/gateway/client.go ReadyWorkItems → sortBySeq; internal/node/loop.go walks the orgs in turn), so an urgent goal's item waits behind a low one's. On 2026-09-24 Chris set priorities on all 28 live goals across runsheet, projectmesh-io, go-apis and wardroom. Claim in priority order: collect ready items across every org the node serves, join each to its goal's priority (one GoalIndex read per org per tick, not per item), and claim urgent first, then high, normal, low; within a priority, oldest evaluation-free item first, then seq. Planning and evaluation are claimed the same way (a planner for an urgent goal first). No starvation guard beyond that for now: note it in the README. The platform already serves the field; this is node-only.

## Acceptance

Tests with the fake gateway: with capacity 1, a ready item of an urgent goal in one org is claimed before an older ready item of a normal goal in another org; empty priority sorts as normal; plans and evaluations follow the same order; one GoalIndex read per org per tick. README documents the order.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | Claim ready work in priority order across orgs | claimed | — | — | $0.00 |
| 2 | Order planning and evaluation by goal priority; document the order | pending | — | — | $0.00 |

## Log

- 2026-09-24 · item 1 · session started
