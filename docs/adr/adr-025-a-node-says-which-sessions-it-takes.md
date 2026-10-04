---
summary: node.yaml's `takes:` names the kinds of session a node claims — a list (`[evaluate]`) or a per-kind cap map (`{evaluate: 3}`), left out every kind — and the loop skips a kind not taken before it lists or asks the platform, read live every tick; the heartbeat will carry the kinds as `holding.kinds` so the platform can name a kind no online node takes.
status: accepted
---

# ADR-025: A node says which sessions it takes

2026-10-04, goal `a-node-says-which-sessions-it-takes` (item 3).

## Context

The Mini is the AI node and sits at load 30 and 80–90°C from sessions
alone: a build runs the repo's tests and so does its evaluation, both on
the one machine. bob (i7-8700K, 12 threads, 62 GB) has been the home CI
machine since 2026-10-03, under its own machine-wide slot limit
(`~/bin/ci-slot.sh`, `~/.ci-slots/limit`). Evaluations are the
test-heavy sessions and belong beside CI on bob; builds, plans and
research stay on the Mini.

Until now every node claimed every kind — `claimWorkItem`,
`claimWorkItemEvaluation`, `claimWorkItemRebase`, `claimPlan`,
`claimDirectionResearch` — and node.yaml could bound the node's
capacity, models and budgets per kind, but not say which kinds it takes.

## Decision

1. node.yaml gains `takes:`, either a list of kinds or a map of kind to
   cap:

   ```yaml
   takes: [evaluate]          # this machine evaluates and nothing else
   takes: {evaluate: 3}       # evaluates, at most three at once
   ```

   The kinds are `build`, `evaluate`, `rebase`, `plan` and `research`;
   any other is refused by `LoadConfig` naming it, and `takes: []` is
   refused (leave the key out instead). **Left out, the node takes every
   kind**, as before.
2. **Skip before listing.** Each claim path — `claimReady` (build, its
   ready and review listings included), `evaluateHeld`,
   `rebaseRequested`, `planRequested`, `researchDue` — returns before it
   lists anything or asks the platform when its kind is not taken
   (`Loop.takesKind`). Nothing is claimed and then released, and a node
   that does not evaluate does not read the evaluating listing at all.
3. **Live.** `takes:` is re-read every tick like the claim limits
   (`Reloaded.Takes`, `Loop.Takes()`), and a change is logged once, at
   the tick it takes effect: `takes: every kind → evaluate`. No restart,
   no drain; a session already running finishes.
4. A per-kind cap bounds that kind inside the node's capacity (item 5 of
   the goal enforces it).

## The platform half

An evaluation waits on the node that built the item. If that node does
not take `evaluate`, nothing on the platform says so today, and the item
waits silently. The heartbeat's holding report should carry the kinds the
node takes as **`holding.kinds`** — a list of kind names, every kind when
node.yaml names none, each with its cap when it has one — so the console's
node page shows them and a goal's unclaimed reason can name a kind no
online node takes. That field, and the platform reading it, are
runsheet/runsheet's half and are not changed here; the node sends it only
once the gateway's schema takes it, introspected like `holding.orgs`.

## Consequences

- An evaluate-only node evaluates nothing until the platform lets it
  claim another node's evaluation: the cross-node evaluation claim is
  item 4 of the goal.
- The per-kind cap (`takes: {evaluate: 2}` under capacity 4) is item 5.
- `mesh node status` printing the kinds taken, and the heartbeat carrying
  `holding.kinds`, are item 6.
- A node that takes `evaluate` still needs what an evaluation does: the
  repos' toolchains, docker or podman for their services, and the
  credentials the platform issues at claim (README, Which sessions a
  node takes).
