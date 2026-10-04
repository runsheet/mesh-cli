---
summary: A node serving several orgs divides its capacity into a floor per org (equal shares, or weights by org name, the largest remainder to the orgs in listed order) and claims first for an org below its floor with ready work, its best item whatever the priority; the slots left go to ADR-017's priority order across orgs. An org's builds, plans and research count toward its floor; evaluations and rebases do not.
status: accepted
---

# ADR-023: Every org gets its share of a node

2026-10-04, goal `every-org-gets-its-share-of-a-node` (item 1). Amends
the claim order of [ADR-017](adr-017-finish-before-you-start.md)
(`claimOrder`, `internal/node/loop.go`); the node's owner and the orgs
it serves are [ADR-015](adr-015-a-node-registers-once-with-an-owner.md)'s.

## Context

A node owned by an enterprise serves every org of it (ADR-015). Its
`claimReady` pooled every served org's ready items into one list and
claimed them by `claimOrder`: goal priority, then finish-first, fresh
before retry, ready longest, seq; full ties kept the order the orgs
were listed in. Nothing divided the node's capacity between orgs. An
urgent goal in one org took every slot for as long as it had ready
items, and at equal priority the org with the oldest ready items won
every tie. A quiet org with a normal goal could wait behind a busy one
indefinitely: the order was fair to goals, not to the orgs sharing the
machine.

## Decision

**Floors.** Each tick the node computes a floor per served org,
`Floors(capacity, scopes, weights)` in `internal/node/shares.go`:

- `capacity` is the capacity in force (`Loop.Capacity()`);
- `weights` is keyed by the org's `Name()` (its login, or its slug when
  it has none); a missing or zero weight is 1 and nil weights are equal
  shares. `Loop.Weights()` answers nil today; node.yaml's `shares:` and
  the owner answer's `shares` feed it in later items of the goal;
- each org gets `capacity × weight / total` rounded down, and the slots
  the rounding leaves go one each to the largest remainders, the orgs
  in listed order on a tie, so the floors sum to the capacity:
  capacity 4 over two orgs is 2 and 2, at weights 3:1 it is 3 and 1,
  capacity 5 is 3 and 2 and capacity 1 is 1 and 0.

**What an org holds.** The loop records, per session, the org it counts
toward (`Loop.sessionOrg`, subject → org id), set where a build, a plan
or a research session is marked — `Loop.start` and `Loop.Adopt` for a
build, `startPlan` and `startResearch` for a plan and a research
(re-opened claims included), `AdoptSessions` for an adopted one by the
org whose node id its `state.json` names — and cleared where the mark
is. `heldByOrg()` counts an org's builds, plans and research.
Evaluations and rebases do not count, as they do not count against
capacity.

**The order.** `claimReady` replaces `claimOrder(ready)` with
`shareOrder(ready, floors, heldByOrg())`, pure:

1. while some org holds fewer sessions than its floor and has ready
   items left, its best item by `claimOrder` goes next — the org
   furthest below its floor first, the orgs in listed order on a tie —
   and counts as held;
2. then everything left, in `claimOrder` across orgs (ADR-017,
   unchanged).

The floor section ignores the goal's priority by design: that is what a
floor is. It does not bypass anything else — the holds, the per-repo
unmerged limit, the account choice, `maxRank` at a ranged node's min and
the urgent-only reserve (`normalFree`) still apply to an item taken for
a floor. A floor is not a ceiling: an org with nothing ready leaves its
slots to the others. A claim made for a floor on a node serving more
than one org logs `<org>: claimed item N "title" (below its floor of F,
holding H)`; the held count is the tick's own, raised with each claim.

**Reporting.** Each served org's share is reported as data, one row per
org in listed order: `org` (its `Name()`), `orgId`, `floor` (from
`Floors` on the capacity in force), `held` (`heldByOrg`: its builds,
plans and research) and `ready` (the ready items its listing returned
on the last claim pass, recorded by `claimReady`). An org whose ready
read failed, or that has not been listed yet, reports `ready: -1`
rather than being left out, so its floor and held still show; the
count is the last listing's, so a tick with no free slot (which lists
nothing) keeps the previous one. The rows ride the heartbeat's holding
report as `holding.orgs` (`gateway.HoldingOrg`, always a list, `[]`
with no org), sent only when the gateway's holding input takes `orgs`
(`takesHoldingOrgs`, introspected and kept an hour like
`holding.unclaimed[].until`; dropped otherwise, the rest of `holding`
sent); `specs/contracts/heartbeatNode.json` documents the field. The
loop hands the same rows to `Loop.Shares` at the end of each pass, and
`mesh node start` leaves them in `~/.mesh/node.shares`
(`RunState.WriteShares`, atomic like `node.scaling`, removed on stop),
which `mesh node status` prints while a node runs here as an `ORG
FLOOR HELD READY` table (`sharesLines`; a failed read's ready as `-`),
or `shares: none recorded` when the file is absent.

## Consequences

- Two orgs at capacity 4, one with urgent work and one with normal:
  each gets 2. With the second org idle the first gets 4.
- A plan or research session takes a floor slot of its org, so an org
  whose director runs at night does not also take its full share of
  builds; it does not take capacity from anyone, since plans and
  research still do not count against capacity.
- At capacity 1 over two orgs the first listed org's floor is the one
  slot: while it has ready work and holds nothing, its item goes before
  another org's urgent one. Weights (a later item) are how an enterprise
  says otherwise.
- An item taken for a floor and then skipped (a hold, no account) is
  not replaced by the org's next item in the floor section this tick;
  the org's other items keep their place in the priority section, and
  the next tick re-computes the floors.
- Plans and evaluations keep their own priority orders (`planOrder`,
  `evaluateOrder`); only build claims are divided.
- `mesh node status` and the heartbeat's holding report show floor,
  held and ready per org (Reporting, above); the `shares` weights are a
  later item of the goal.
