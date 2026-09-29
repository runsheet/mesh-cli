---
summary: At equal priority a ready item with work already begun on a branch — an open pull request, an evaluation, a rebase conflict — is claimed before a fresh one while it has fewer than two failed evaluations; then the item ready longest, then seq; evaluateOrder and rebaseOrder are left as they are.
status: accepted
---

# ADR-017: Finish before you start

2026-09-29, goal `finish-before-you-start` (item 1). Amends the claim
order the README's "Start" section describes (`claimOrder`,
`internal/node/loop.go`).

## Context

`claimOrder` sorted ready items by their goal's priority, then **fresh
items before retries** (a retry carries an evaluation), then seq. At
equal priority an item already built, reviewed and sent back queued
behind every brand-new item, and work in progress accumulated.

Measured on 2026-09-29: **22 items ready against 5 in flight**;
`runsheet/runsheet` at **13 open pull requests** against a merge-queue
cap of 7, with two items cancelled outright for `merge queue full: 15 of
7`; and item #1075 of `no-pr-outlives-its-goal` `ready` since
2026-09-28T02:24Z — **35 hours** — with a rebase conflict recorded in
full and newer items claimed ahead of it the whole time. Nothing was
broken; the queue did what it was written to do.

The listed `gateway.WorkItem` already carries what the node needs to
tell started work from fresh — `PRURL`, `Attempts`, `Evaluation`,
`EvaluationPassed`, `RebaseState`, `RebaseConflict` — so this is a
change to how the node orders what it is given. The one field added is
`readyAt`, which the platform's schema already serves; it is selected
as optional (`readyAt?`, [ADR-010](adr-010-generated-gateway-fields.md)),
so an older platform lists items without it and the order falls to seq.

Fresh-first had a reason: preferring retries unconditionally lets a
poisoned item — one whose evaluation always fails — cycle at the head of
the queue and starve everything behind it. The preference has to be
bounded, or starvation in one direction is traded for starvation in the
other.

## Decision

The node claims ready items from every org it serves in this order:

1. **Priority.** By the goal's priority: urgent, high, normal, low. A
   goal with no priority, an unknown one, or one whose goals could not be
   read this tick, is normal. Priority outranks every step below: an
   urgent fresh item goes before a normal retry.
2. **Finish first.** At equal priority, an item in the *finish-first
   class* goes before every item that is not. An item is in the class
   when it carries started work and is under the attempt bound:

   ```
   startedWork(it) = it.prUrl != ""                       // an open pull request: sent back for
                                                          // rework, a rebase, or re-evaluation
                  || it.evaluation != ""                  // a retry after a failed evaluation
                  || it.rebaseConflict != ""              // a conflict with its base
                  || it.rebaseState not in {"", "none"}   // a rebase requested, claimed, unresolved…
   finishFirst(it) = startedWork(it) && it.attempts < 2   // finishFirstAttempts
   ```
3. **The attempt bound: `finishFirstAttempts = 2`.** `attempts` counts
   failed evaluations: the platform adds one on each verdict that did not
   pass, leaves it alone on a release, and fails the item at three
   (mesh-specs trd, the item's lifecycle). The platform's cap of three is
   the anchor; the node's bound is one under it. An item with two failed
   evaluations is on its last try, so it no longer displaces fresh work:
   a poisoned item takes the head of its priority's queue at most twice,
   then waits behind the fresh items, and the platform fails it on its
   third failure. A bound of three would never bind (the item is failed
   first); a bound of one would give an item sent back once for rework no
   preference at all, which is the case this goal is for.
4. **Fresh before bounded-out retries.** Among the items not in the
   class, fresh items go before retries (an item with an evaluation), as
   they did before: a retry at the bound keeps its old place, behind
   fresh work, and never blocks it. Within the finish-first class this
   step does not apply — a rework retry, a rebase and a re-evaluation are
   one class.
5. **Ready longest.** Within the step's class, the item with the older
   `readyAt` goes first, when both carry one. The platform serves a
   `Time` as RFC 3339 with however many fractional digits it has, which
   does not sort as a string, so the node parses both and compares the
   times. An item with no `readyAt` (an older platform) or one that does
   not parse falls to seq against the rest.
6. **Seq.** Then the lower seq first.
7. **Stable.** The sort is stable (`sort.SliceStable`): items tied on
   every step keep the order the orgs and their items were listed in.

### `evaluateOrder` and `rebaseOrder` are left alone

`evaluateOrder` sorts the items this node holds for evaluation by
priority, then seq. The same argument was considered and does not apply:
every held item is one this node itself finished, so the finish-first
class does not tell them apart — each is started work; evaluations are
not capacity-gated, they run whenever an account has headroom, so there
is no queue in which a fresh start could displace them; and the item
carries no time it entered `evaluating`, so there is no age to break
ties on. `rebaseOrder` orders items whose rebase the platform requested
— all started work by definition — and is left as it is for the same
reason.

## Consequences

- An item sent back for rework, a rebase or a re-evaluation is claimed
  before a fresh item of its priority, so started work lands before new
  work opens more pull requests.
- The item ready longest in its class goes first: nothing waits 35 hours
  behind newer work of its own class and priority.
- A poisoned item costs at most two turns at the head of its priority's
  queue; after that it waits behind fresh work until the platform fails
  it. `TestLoopPoisonedItemDoesNotStarveTheQueue` proves it with capacity
  1 and an evaluation that always fails.
- Priority stays strict: a steady stream of urgent work still keeps a
  low goal's items waiting, as before.
- An older platform without `readyAt` loses only step 5.

## Per-repo limit on unmerged work

Added by the next item of goal `finish-before-you-start`.
