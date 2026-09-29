---
summary: At equal priority a ready item with work already begun on a branch — an open pull request, an evaluation, a rebase conflict — is claimed before a fresh one while it has fewer than two failed evaluations; then the item ready longest, then seq; evaluateOrder and rebaseOrder are left as they are. node.yaml's claim.unmerged_per_repo (default no limit) holds fresh claims for a repo with that many items in review, counted from the node's own review listing rather than RepoIndex.reviewPile, while started work there is still claimed.
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
   `readyAt` goes first. The platform serves a `Time` as RFC 3339 with
   however many fractional digits it has, which does not sort as a
   string, so the node parses both and compares the times. An item with
   no `readyAt` (an older platform) or one that does not parse goes
   after every item of its class that has one — as if it became ready
   now — and among themselves such items go by seq. The key is
   (has a time, the time, seq). Comparing times only when both items
   carry one and seq otherwise was the first version and is not an
   order: older A (seq 3) before newer B (seq 1), B before timeless C
   (seq 2), C before A — a cycle, and a sort given one guarantees
   nothing.
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
- An older platform without `readyAt` loses only step 5: every item
  lacks the time, so the order falls to seq.

## Per-repo limit on unmerged work

Added by item 2 of goal `finish-before-you-start` (PR #465), carried onto main by item 4.

The claim order decides which work goes first; it does not stop the node
opening a pull request that cannot land. On 2026-09-29
`runsheet/runsheet` had 13 open against a merge-queue cap of 7 and two
items were cancelled for `merge queue full: 15 of 7`: work was started
that could not merge, and it took slots from work that could.

**The key.** node.yaml `claim: {unmerged_per_repo: N}` (`ClaimConfig`).
Its default is no limit: left out or 0 keeps the claim as it was
before, and then no `review` listing is read at all. A negative value is
refused. It rides `Reloaded.Claim` and `Loop.reload` applies it every
tick like `context:`, so a change takes effect on the next tick without
a restart and is logged (`claim: unmerged_per_repo 0 → 7`).
`Loop.UnmergedPerRepo` is the value in force.

**What counts as unmerged.** An item in status `review` — the TRD's
status for an item whose pull request is open and waiting to merge —
counted by its `repoId`. `claimReady` reads each served org's `review`
items once per tick (`OrgWorkItems(org, "review")`), only while the key
is set and only on a tick where some org has something ready — but then
*every* served org's, including one with nothing ready or whose ready
listing failed, before any repo is judged: a repo's pull requests may
be in review under another org than the one offering its fresh item, and
counting only the orgs with ready work would let the node claim past the
limit. A fresh item claimed in the tick is added to its repo's count at
once, so one tick never claims more fresh items of a repo than the limit
leaves. A failed listing leaves the counts unknown — any repo may have
review items in that org — so every fresh item is held this tick
(reason `repo <repoId>'s unmerged pull requests could not be counted
(<orgs>'s review items unread) under node.yaml's
claim.unmerged_per_repo <limit>`) and started work is still claimed, as
at the limit. Counting an unread org
as having none would let a repo past its limit take fresh claims on a
slow read; holding fresh work one tick costs a tick.

**What it holds and what it still claims.** A fresh item
(`!startedWork`) of a repo at or past the limit is not claimed and stays
`ready`. Started work in that repo — an open pull request sent back for
rework, a rebase conflict, a re-evaluation — is still claimed, whatever
the count: it finishes a pull request that already counts, and holding
it would deepen the jam the limit exists to clear. The attempt bound of
the class order does not apply here: an item past it is still started
work and still exempt, since claiming it opens no new pull request.
Other repos are unaffected.

**Why it waits.** The held item is recorded with `passOver`, so the
heartbeat's holding report carries it with the reason `repo <repoId>
has <n> unmerged pull requests, at node.yaml's claim.unmerged_per_repo
<limit>`; node.log says it once per repo per tick, not once per item.
The report keeps one entry per goal and kind, so when repos of one goal
are held for different reasons each is appended to the entry's reason
once: two repos at their limit in one goal are both named.

**`reviewPile` and `mergePressureAt`: not read.** The platform's
`RepoIndex` carries both, and today nothing in mesh-cli reads either —
this decision leaves it so. The node counts from its own `review`
listing instead because:

- the count is exact for the items the scheduler will actually merge —
  the items in `review` are precisely the pull requests waiting for the
  integrator;
- it needs no new gateway read and no new field: `OrgWorkItems` and
  `repoId` are already selected, so there is nothing to add to
  [ADR-010](adr-010-generated-gateway-fields.md)'s field lists and no
  platform change to land first;
- `reviewPile`'s definition and `mergePressureAt`'s trigger live on the
  platform and are not documented in this repo, so a limit built on
  them would mean whatever the platform later decides they mean.

If the platform documents them and the node needs a signal it cannot
derive from `review` (a pile the node does not list, or a pressure the
integrator measures), this section is the place to revisit.
