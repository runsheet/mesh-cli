---
title: Goal — A node takes a rebase the integrator could not do itself
kind: plan
status: draft
parent: prd
---

# Goal — A node takes a rebase the integrator could not do itself

Mesh goal `node-runs-rebase-sessions`, running.

## Statement

2026-09-24: goal merge-queue made the integrator rebase a conflicting PR in-process when the conflict is mechanical, and otherwise RequestWorkItemRebase — asking a node for a session of kind rebase (ADR 008, trd §6a; ClaimWorkItemRebase / FinishWorkItemRebase / FailWorkItemRebase). mesh-cli has no rebase session, so six PRs (runsheet#306, #310, #325, console#76, #82, website#5) sat conflicting with rebase_state requested and nothing moving, until FailWorkItemRebase was sent by hand through /ops, handing each conflict to its builder. Node half: the node lists items with rebase_state requested in the orgs it serves (claimed in priority order with the rest of its work, see node-claims-by-priority), claims one with ClaimWorkItemRebase, runs a session whose whole job is: check out the item's branch, rebase onto the base, resolve the conflicts keeping both sides' intent (the mesh-branch-rebase skill in runsheet/runsheet is the runbook), run the item's acceptance checks, push with --force-with-lease, and FinishWorkItemRebase with the new head and the files it resolved — or FailWorkItemRebase with why, which hands the item to its builder. It does not count against capacity the way an evaluation does not. The gateway already serves the commands and the rebase_state field.

## Acceptance

Tests with the fake gateway: a requested rebase is claimed once, runs a rebase session, and finishes with the pushed head; a session that cannot resolve fails the rebase with its reason; two nodes never both claim one; rebase sessions follow the priority order. README documents the kind.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | Gateway client and fake: rebase_state, claim, finish, fail | claimed | — | — | $0.00 |
| 2 | The rebase session: prompt, runner, loop wiring, priority order | pending | — | — | $0.00 |
| 3 | Document the rebase session kind | pending | — | — | $0.00 |

## Log

- 2026-09-24 · item 1 · session started
