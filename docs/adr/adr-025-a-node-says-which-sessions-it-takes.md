---
summary: node.yaml's `takes:` names the kinds of session a node claims — a list (`[evaluate]`) or a per-kind cap map (`{evaluate: 3}`), left out every kind — and the loop skips a kind not taken before it lists or asks the platform, read live every tick, a capped kind held at its cap and named on the holding report; the heartbeat carries the kinds, names alone, as `holding.kinds` so the platform can name a kind no online node takes; an evaluating node claims another node's evaluation with `claimWorkItemEvaluation` when the platform has it.
status: accepted
---

# ADR-025: A node says which sessions it takes

2026-10-04, goal `a-node-says-which-sessions-it-takes` (item 3; the
cross-node evaluation claim, item 4; the cap, decision 4, item 5).

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
4. **The cap.** A per-kind cap bounds that kind inside the node's
   capacity: `takes: {evaluate: 2}` under capacity 4 runs at most two
   evaluations at once. `Loop.runningOf(kind)` counts the loop's own
   map of the kind (`running` for build, `evaluating`, `rebasing`,
   `planning`, `researching`) and `Loop.kindRoom(kind)` is the cap less
   that. Each claim path reads the room once at its start and lowers it
   with each start it makes (`kindCap`), so a tick never overshoots —
   not even when a session ends within the tick. For build the Tick
   lowers `free` to the room before `claimReady`; for the kinds capacity
   does not bound the cap is the only bound. Work the cap holds back is
   passed over, once per goal and kind, with the reason `at node.yaml's
   takes cap of N <kind>(s)` (`evaluation(s)` for evaluate) on the
   holding report's `unclaimed`, and logged once per kind per tick:
   `takes: evaluate at its cap of 2; 3 waiting`. It is not claimed and
   released: an evaluation stays in evaluating, an item ready, a goal in
   planning.

## The platform half

An evaluation waits on the node that built the item. If that node does
not take `evaluate`, nothing on the platform says so today, and the item
waits silently. The heartbeat's holding report carries the kinds the
node takes as **`holding.kinds`**, the field the platform half reads: a
list of kind names (`[]string`) and nothing else — no per-kind cap — in
the fixed order build, evaluate, rebase, plan, research, every kind when
node.yaml names none, always a list (`["evaluate"]` for `takes:
[evaluate]` and for `takes: {evaluate: 2}` alike). A cap stays the node's
own business: it shows on `mesh node status` (`Takes: evaluate (at most
2)`) and, when it holds work back, on `holding.unclaimed`'s reason. With
the kinds the console's node page shows them and a goal's unclaimed
reason can name a kind no online node takes. The node sends `kinds` only
when the gateway's `HeartbeatNodeInput` `holding` input type has the
field (`takesHoldingKinds`, introspected and kept `ScalingProbeEvery`
like `holding.orgs`), dropping it otherwise; the holding summary says
the same in words either way, opening `takes <kinds>; ` when fewer than
every kind is taken (`takes evaluate; normal: up to 4 item(s)`).
`specs/contracts/heartbeatNode.json` shows the field. The platform
reading it is runsheet/runsheet's half and is not changed here.

## Consequences

- **The cross-node claim** (item 4 of the goal): `evaluateHeld` takes
  every item in `evaluating` of a served org. One this node holds is
  evaluated with no claim, as before; one another node holds, or none,
  is claimed with `claimWorkItemEvaluation` (`GW.ClaimWorkItemEvaluation`)
  before the session starts — marked in the loop's evaluating set first
  and cleared on a refusal, as `rebaseRequested` does, the refusal the
  ordinary answer and not logged, any other error logged — then read
  again, so the item reaches the evaluator (and an adoption after a
  restart) with this node's `nodeId`. The claim is asked only when the
  gateway's `Mutation` has `claimWorkItemEvaluation`
  (`TakesEvaluationClaim`, introspected and kept an hour); without it
  the node evaluates only what it holds, said once per run: `the
  platform takes no evaluation claim; evaluating only what this node
  holds`. A node that builds and evaluates claims other nodes'
  evaluations too: the platform's claim is the lock. **Amended
  2026-10-14** (goal an-items-evaluation-runs-on-one-node): the builder
  claims its own item's evaluation too. Left unclaimed, a builder
  evaluated its item while another node, seeing `evaluationNodeId`
  empty, claimed and evaluated it as well, and the loser's verdict was
  refused `not yours` (dl360, 2026-10-15, item
  a-goal-is-done-when-its-last-item-merges/1). Now, when the gateway
  takes the claim, every item is claimed before its session starts,
  the builder's own included — the platform converges for the same
  node — so the claim is the one lock. A refusal on an item this node
  built means another node has it: the item leaves the loop's
  evaluating set, no session starts, and it is said once per item, not
  as an error, `item <goal>/<seq>: evaluation is node <x>'s; not
  evaluated here`. A platform without the claim keeps the old rule —
  own items only, with no claim — and an adopted evaluation after a
  restart is already the loop's and is not claimed again.
- The per-kind cap (`takes: {evaluate: 2}` under capacity 4) is item 5
  (amended above, decision 4).
- `mesh node status` prints the kinds taken as `Takes: <kinds>` after the
  `This machine:` line (`Takes: every kind`, `Takes: evaluate (at most
  2)`), and the heartbeat carries `holding.kinds` (above): item 6.
- A node that takes `evaluate` still needs what an evaluation does: the
  repos' toolchains, docker or podman for their services, and the
  credentials the platform issues at claim (README, Which sessions a
  node takes).
