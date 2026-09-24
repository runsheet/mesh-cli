---
title: Goal — A node that cannot pay for a session stops, instead of failing items
kind: plan
status: draft
parent: prd
---

# Goal — A node that cannot pay for a session stops, instead of failing items

Mesh goal `node-out-of-credit`, running.

## Statement

2026-09-23 18:10–21:00Z: the mini's Claude account hit its monthly spend limit, and for three hours every session ended at once with "You've hit your monthly spend limit · raise it at claude.ai/settings/usage" — 8,813 times in node.log. Each was treated as the item's own failure: 8 items failed across 5 goals, each goal spent a re-plan, one planner was retried every few seconds for three hours, and nobody was told. Detect the harness's out-of-credit and rate-limit messages (and a non-zero exit with no output twice running) as the node's condition, not the item's: release the claim without counting an attempt, stop claiming new work, report it on the node's heartbeat (a harness_unavailable status and reason, which the platform turns into one needs_person row — coordinate the field with the platform), and probe once every ten minutes with a minimal session, resuming on success.

## Acceptance

Tests: a session ending with the spend-limit message releases its item with attempts unchanged and puts the node in harness_unavailable; no new claims while in it; a successful probe resumes claiming; the heartbeat carries the reason. README: what an operator sees and does.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | Spec: the node's harness-unavailable condition — ReleaseWorkItem, HeartbeatNode status/reason, detection and the probe | merged | mesh/node-out-of-credit-1 | https://github.com/projectmesh-io/mesh-specs/pull/9 | $0.56 |
| 2 | harness: classify a session's exit as out-of-credit/rate-limited or silently failed | claimed | — | — | $0.00 |
| 3 | gateway: ReleaseWorkItem and a HeartbeatNode that carries status and reason | claimed | — | — | $0.00 |
| 4 | node: stop on harness-unavailable — release, gate claiming, heartbeat the reason, probe and resume | pending | — | — | $0.00 |

## Log

- 2026-09-24 · item 3 · session started
