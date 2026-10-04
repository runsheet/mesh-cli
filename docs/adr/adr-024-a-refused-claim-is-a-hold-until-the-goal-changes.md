---
summary: A ClaimWorkItem refused for budget, pause or approval holds the item's goal on the node — not asked again, passed over on holding.unclaimed and listed as a held goal of kind item — until the goal's row changes (its updatedAt, else its status and paused reason) or the refused item leaves the ready listing for a tick and is listed again; any other refusal is retried as before.
status: accepted
---

# ADR-024: A refused claim is a hold until the goal changes

2026-10-04, goal `a-refused-plan-is-released-not-replanned` (item 4).
Numbered 024 beside `adr-024-a-session-commits-as-its-agent.md`, which
landed first; cite this one as "ADR-024, a refused claim is a hold".

## Context

On 2026-10-03 the Mini asked the platform to claim one item of a
budget-paused goal every 70 seconds — 285 claims, each refused with
`goal is paused: budget exhausted`, each logged as `not ours`. The
platform keeps listing a paused goal's items as ready (the pause is on
the goal, not the item), so the ready read offers the item every tick
and nothing on the node remembered why the last claim failed. The
refusal is not transient: it holds until a person raises the budget,
lifts the pause or approves, and each of those is a change to the
goal's row.

## Decision

`claimHoldKind(msg)` classes a `ClaimWorkItem` refusal by lowercase
whole-word match: `budget` (budget, over budget, spent), `pause`
(paused, pause), `approval` (approval, approve, checkpoint, awaiting a
person), tried in that order, else `""`. A classed refusal in
`claimReady`:

- reads the goal from the tick's one GoalIndex read (`Loop.goals`) and
  notes it in `Loop.ClaimHolds` (`ClaimHolds.Note`, keyed by goal id:
  kind, reason, the refused item, the row's `updatedAt`, status and
  paused reason);
- records it with `Loop.NoteHeld(HeldGoal{Kind: "item", Reason:
  "<kind>: <message>"})`, so `node.held` and `mesh node status` list it;
- logs once `claim: <org> goal <slug> held (<kind>): <message>; not
  asked again until the goal changes`.

On later ticks, before an account is chosen, an item whose goal
`ClaimHolds.Holds` is passed over silently as `item` with the reason
(`passOver`, no until — the hold has no clock). The hold lifts when:

- the goal's `updatedAt` differs from the noted one — or, on an older
  platform that serves no `updatedAt`, its status or paused reason; or
- the refused item was absent from a tick's ready listing and is listed
  again (`ClaimHolds.Unlisted` marks it each tick; a tick whose listing
  of any org failed marks nothing, since unread is not absent).

A lifted hold is cleared (`ClaimHolds.Clear`, `Loop.ClearHeld`), logged
`claim: <org> goal <slug> hold lifted`, and the item is claimed on that
same tick. An unclassed refusal — `item is claimed, not ready`, another
node's win — keeps today's `not ours` line and is asked again next
tick.

## Consequences

- One claim per paused goal, not one per tick: five simulated hours at
  70 seconds ask once (`claimhold_loop_test.go`).
- The hold is in memory: a restarted node asks once more and holds
  again. That is one refused claim per start.
- The hold rests on wording. A refusal the platform rewords out of the
  three classes is retried as before — the old cost, never a lost item;
  a typed refusal code on the platform would replace the word match.
- A goal row changed for an unrelated reason (a title edit) lifts the
  hold early: one more refused claim, then held again.
