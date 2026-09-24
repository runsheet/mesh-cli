---
title: Goal — A node drains instead of dying, and reads its config live
kind: plan
status: draft
parent: prd
---

# Goal — A node drains instead of dying, and reads its config live

Mesh goal `node-drains`, awaiting_approval.

## Statement

On 2026-09-23 the mini's capacity went from 3 to 6 and the go-apis org was added, and neither reached the node for an hour: the node reads capacity and resolves its orgs only at start, has no drain, and a restart kills its running claude sessions. A watcher waiting for zero sessions never saw one (planners and evaluators don't count against capacity, so the node is rarely idle), and the node was finally restarted by hand mid-session. Give mesh-cli: (1) a drain — SIGUSR1 or `mesh node drain` stops new claims, lets running sessions finish, then exits so systemd restarts it on the new binary/config; (2) capacity and the org list re-read on each tick (node.yaml and MyOrgs), so neither needs a restart; (3) `mesh node status` shows draining.

## Acceptance

Tests: a draining loop claims nothing new and exits after the last running session ends; a capacity change in node.yaml takes effect on the next tick; a newly claimed org is served on the next tick without restart. README documents swapping the binary with a drain instead of a restart.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | TRD §3: document drain and live config reload | failed | — | — | $0.00 |
| 2 | mesh node drain: SIGUSR1 stops new claims, running sessions finish, then the process exits | failed | — | — | $0.00 |
| 3 | Capacity and the served org list are re-read every tick, not just at start | cancelled | — | — | $0.00 |
| 4 | TRD §3: document drain and live config reload | failed | — | — | $0.00 |
| 5 | Capacity and the served org list are re-read every tick, not just at start | failed | — | — | $0.00 |
| 6 | mesh node drain: SIGUSR1 or `mesh node drain` stops new claims, running sessions finish, then the process exits | cancelled | — | — | $0.00 |
| 7 | Capacity and the served org list are re-read every tick | claimed | — | — | $0.00 |
| 8 | mesh node drain: SIGUSR1 or the command stops new claims, sessions finish, process exits; status shows draining | pending | — | — | $0.00 |
| 9 | TRD §3: document drain and live config reload | pending | — | — | $0.00 |

## Log

- 2026-09-24 · item 7 · session started
