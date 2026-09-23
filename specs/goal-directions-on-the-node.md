---
title: Goal — Directions on the node: a research session belongs to a direction
kind: plan
status: draft
parent: prd
---

# Goal — Directions on the node: a research session belongs to a direction

Mesh goal `directions-on-the-node`, running.

## Statement

On 2026-09-23 the platform finished moving a project's director onto a `Direction` aggregate of its own — several per project, each with its own words, window, timezone, gap, ceiling and research claim. `DirectionIndex`, `ClaimDirectionResearch` and `RecordDirectionResearch` are all built, deployed and proven.

The node was never ported. `gateway.Project` still carries `Direction`, `DirectorEnabled` and `MaxLiveGoals` as `json:"-"` stubs that are always zero, so `researchDue` skips every project silently, and `TestLoopResearchesDirectedProjects` and `TestLoopSkipsAFullProject` are skipped with "the node's director is being ported to DirectionIndex".

The consequence is that **no project researches anything, anywhere** — wardroom's director has been dead since the deploy, and a direction written on any project is words nothing reads. This goal is what makes directions real.

## Acceptance

A node with a direction enabled on a project inside its window claims that direction, runs one research session on its words and hands the proposals in against the direction. Two directions on one project research independently on their own hours. The two skipped loop tests run and pass. Proven with a fake gateway in the node's own tests, and by wardroom's director producing a proposal again after the node is upgraded.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | The node lists directions and claims one, not a project | claimed | — | — | $0.00 |
| 2 | A research session belongs to a direction, and two on one project run on their own hours | pending | — | — | $0.00 |

## Log

- 2026-09-23 · item 1 · session started
