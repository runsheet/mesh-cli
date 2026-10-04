---
summary: The plan path keeps each goal's last plan-check refusal; a second identical (normalised) refusal releases the plan claim with the refusal's text as the reason (releaseGoalPlan), runs no third plan session, and holds the goal on the node until its row's updatedAt changes.
status: accepted
---

# ADR-023: A refused plan is released, not re-planned

2026-10-04, goal `a-refused-plan-is-released-not-replanned` (item 3).
Numbered 023 beside the other ADR-023s that landed the same week; cite
this one as "ADR-023, a refused plan is released, not re-planned". It
amends ADR-021's plan check.

## Context

2026-10-03: the loom goal held one failed item, and every plan of it
recreated that item at its acceptance — on the ask and on the re-ask.
The plan check (ADR-021) refused each final answer with the same
sentence, `plan would re-land failed item … at the same acceptance;
refused`, and nothing handed in left the goal in planning. The next
tick claimed it again. Six plans in 30 minutes, each two Fable sessions
at up to $5 a plan, and the platform was never told: the refusal was a
node.log line and nothing else. `MaxPlanAttempts` did not stop it — its
count lives on a `Planner`, and `mesh node start` builds one per plan.

A planner that answers the same refused plan twice will answer it a
third time. Only a person — amending the goal, its acceptance or its
failed item — changes the answer.

## Decision

- **The last refusal per goal.** `readPlan`'s final refusal is a typed
  error, `*PlanRefused{Goal, Reason}` (`PlanRefusedOf`), its reason the
  joined refusal text. The planner keeps each goal's last refusal,
  normalised (`normaliseRefusal`: lower-case, whitespace collapsed,
  trailing punctuation dropped), under the work root
  (`PlanRefusalsPath`, `.plans/refusals.json`), so a fresh `Planner`
  and a restarted node read the same record.
- **A second identical refusal releases the claim.** When a refusal
  normalises to the goal's last one, the node calls
  `releaseGoalPlan(goal, nodeId, reason)` with the refusal's text
  verbatim, logs `goal <slug>: plan refused twice with the same reason;
  released, not re-planned: <reason>`, records the goal released with
  its row's `updatedAt` (re-read after the release, so the claim and the
  release touching the row are not the goal changing), and counts no
  further try. A platform without `releaseGoalPlan` is said once
  (`platform has no releaseGoalPlan; held on the node`) and the goal is
  still released on the node.
- **Held until the goal changes.** `Planner.Released(goal)` is true
  while the goal is recorded and its `updatedAt` equals the recorded
  one; a platform that serves no `updatedAt` is judged by status alone,
  released until the goal leaves `planning`. `planRequested` asks it
  (`Loop.PlanReleased`) before an account or a claim: a released goal is
  passed over on `holding.unclaimed` with the reason and recorded as a
  `HeldGoal` of kind `plan` — `node.held`, `mesh node status` — cleared
  once `Released` turns false. A changed goal forgets its release and its
  last refusal: it is planned afresh, and released again only after two
  more identical refusals.
- **The platform half** — holding the goal for a person on a released
  plan, and showing the reason — is goal
  `a-released-plan-holds-for-a-person` in runsheet.

## Consequences

- A goal whose plans keep re-landing costs two plans, not one a tick;
  the refusal reaches the platform as the release's reason rather than
  only node.log.
- A refusal that differs from the last — a different item, a different
  lineage — is not a repeat: the goal is planned again, and the record
  keeps the newer one.
- The record is the node's: two nodes each spend two plans before each
  holds the goal, until the platform half holds it for everyone.
- `plan_refused_replay_test.go` replays the loom log: six requested
  plans across a restart run sessions for two, one `releaseGoalPlan`
  carries the refusal, no `claimPlan` follows it, and a later
  `updatedAt` plans the goal again.
