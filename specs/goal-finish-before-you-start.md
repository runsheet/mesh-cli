---
title: Goal — Finish before you start: an item with an open pull request is claimed before a fresh one, and a repo has a limit on unmerged work
kind: plan
status: draft
summary: At equal priority, started work is claimed before fresh work (bounded by attempts), and a node.yaml limit holds fresh claims for a repo already carrying too many unmerged pull requests; evaluateOrder and rebaseOrder are left as they were.
parent: prd
---

# Goal — Finish before you start: an item with an open pull request is claimed before a fresh one, and a repo has a limit on unmerged work

Mesh goal `finish-before-you-start`, running.

## Statement

`claimOrder` preferred fresh items over retries at equal priority, so
work already built, reviewed and sent back queued behind every
brand-new item — work in progress accumulated, and `runsheet/runsheet`
ran a merge-queue jam of open pull requests it could not land. Measured
on 2026-09-29: 22 items ready against 5 in flight; `runsheet/runsheet`
at 13 open pull requests against a merge-queue cap of 7; an item ready
35 hours with newer items claimed ahead of it the whole time. Nothing
was broken — the queue did what it was written to do.

Two changes: at equal priority, an item with started work (an open
pull request, a failed evaluation, a rebase) is claimed before a fresh
item, bounded by an attempt cap so a poisoned item cannot starve the
queue; and node.yaml gets a limit, `claim.unmerged_per_repo`, that
holds fresh claims for a repo already carrying that many unmerged pull
requests, while started work in that repo is still claimed. Age breaks
ties within a class so nothing starves behind newer work of its own
class and priority.

## Acceptance

`claimOrder` claims a started item before a fresh one at equal
priority, bounded by attempts, then by the older `readyAt`, then seq,
stably. A poisoned item is proved not to starve the queue. node.yaml's
`claim.unmerged_per_repo`, re-read every tick, holds fresh claims for a
repo at the limit but still claims started work there; its absence
keeps today's behaviour. `evaluateOrder` is either changed with the
same argument or left alone with the reason recorded. An ADR records
the claim order in full. Evidence from the mini, against the
2026-09-29 baseline of 22 ready, 5 in flight, 13 open PRs on
`runsheet/runsheet`, oldest ready 35 hours — naming what moved. Full
acceptance: `docs/adr/adr-017-finish-before-you-start.md` and the goal
record on Mesh.

## Items

| Seq | Title | Branch | PR | Outcome |
| --- | --- | --- | --- | --- |
| 1 | Node: finish before you start — at equal priority an item with started work is claimed before a fresh one, bounded by attempts, oldest readyAt first; ADR-017 records the claim order in full | mesh/finish-before-you-start-1 | [runsheet/mesh-cli#458](https://github.com/runsheet/mesh-cli/pull/458) | evaluation passed (1 attempt); in review, not yet merged |
| 2 | Node: a limit on unmerged work per repo — node.yaml `claim.unmerged_per_repo`, read live, holds fresh claims for a repo at the limit but never the rework of an item already there; ADR-017 amended | mesh/finish-before-you-start-2 | [runsheet/mesh-cli#465](https://github.com/runsheet/mesh-cli/pull/465) | evaluation passed (2 attempts, sharpened to opus after the first); in review, not yet merged |
| 3 | Goal doc: finish-before-you-start — the claim order as it now stands, the evaluateOrder decision, and the mini's ready/in-flight, open PRs per repo and oldest readyAt against the 2026-09-29 baseline | mesh/finish-before-you-start-3 | — | this item |

## The claim order now

From `docs/adr/adr-017-finish-before-you-start.md` (this branch), the
node claims ready items from every org it serves in this order:

1. **Priority.** Urgent, high, normal, low; a goal with no priority, an
   unknown one, or one whose goals could not be read this tick, is
   normal. Priority outranks every step below.
2. **Finish first.** At equal priority, an item in the *finish-first
   class* goes before every item that is not. An item is in the class
   when it carries started work — an open pull request, a failed
   evaluation, a rebase conflict, or a rebase state other than none —
   and is under the attempt bound.
3. **The attempt bound: `finishFirstAttempts = 2`.** `attempts` counts
   failed evaluations; the platform fails an item at three. An item
   with two failed evaluations no longer displaces fresh work: it takes
   the head of its priority's queue at most twice, then waits behind
   fresh items, and the platform fails it on its third failure.
4. **Fresh before bounded-out retries.** Among items not in the class,
   fresh items go before retries, as before.
5. **Ready longest.** Within a step's class, the item with the older
   `readyAt` goes first, both times parsed and compared (not sorted as
   strings); an item with no `readyAt`, or one that fails to parse,
   goes after every item of its class that has one, and among such
   items by seq. The key is (has a time, the time, seq) — a total
   order, unlike comparing times only when both items carry one.
6. **Seq.** Then the lower seq first.
7. **Stable.** Items tied on every step keep the order the orgs and
   their items were listed in.

**The per-repo limit** (item 2): node.yaml `claim: {unmerged_per_repo:
N}` (`ClaimConfig`), re-read every tick with no restart needed. Its
default is no limit: left out or 0 keeps today's behaviour and reads no
`review` listing. A fresh item of a repo at or past the limit is not
claimed; started work in that repo — rework, a rebase, a
re-evaluation — is still claimed whatever the count, since it finishes
a pull request that already counts rather than opening a new one. The
count is the node's own `review` listing across every served org (not
`RepoIndex.reviewPile` or `mergePressureAt`, which the platform carries
but does not document in this repo); a failed listing holds every fresh
item of an affected repo for that tick rather than risk claiming past
the limit.

## evaluateOrder

**Left alone.** ADR-017 records the reason: every item this node holds
for evaluation is one it finished itself, so the finish-first class
does not distinguish them — each is already started work. Evaluations
are not capacity-gated; they run whenever an account has headroom, so
there is no queue in which a fresh start could displace one. And the
item carries no time it entered `evaluating`, so there is no age to
break ties on. `rebaseOrder` is left alone for the same reason: every
item it orders is started work by definition. Neither was changed
silently — both are named and reasoned about in the ADR.

## Evidence

Measured via the mesh-context tools (`mesh_health_flow`,
`mesh_health_stuck`, `mesh_health_nodes`) against the served org
`runsheet`, since the baseline itself spans the projects the mini
serves (the item cited in the baseline, `no-pr-outlives-its-goal`'s
item 2, ready since 2026-09-28T02:24Z, lives in project
`runsheet/runsheet`, not `runsheet/mesh`).

| Date | Mini's node version | Ready | In flight (claimed/running/evaluating) | Open PRs per repo | Oldest ready age |
| --- | --- | --- | --- | --- | --- |
| 2026-09-29 (baseline) | — | 22 | 5 | `runsheet/runsheet`: 13 (merge-queue cap 7) | 35 hours |
| 2026-09-29T15:53Z (measured) | v0.2.191 (desired v0.2.191; latest v0.2.192; 4 commits behind main) | 6 | 7 | `runsheet/runsheet`: 7; `runsheet/mesh-cli`: 4 | 37.5 hours (2249 min; same item, `no-pr-outlives-its-goal` seq 2, still ready since 2026-09-28T02:24Z) |

`runsheet/runsheet`'s open pull requests fell from 13 to 7 — exactly
the baseline's merge-queue cap — while the item that motivated this
goal is still sitting ready, now for 37.5 hours instead of 35, newer
items still claimed ahead of it: the drop is the integrator clearing
its own queue, not this goal's change, since neither item has merged
to main yet (see below).

The mini runs **v0.2.191**, 4 commits behind `main`'s head
(`v0.2.192`/`10a9ba2`); it does **not** yet include items 1 or 2's
merges — both `runsheet/mesh-cli#458` and `#465` are still in review,
not merged, so neither `claimOrder`'s finish-first class nor
`claim.unmerged_per_repo` is running on the mini. This measured row is
therefore a second baseline, not an after-measure.

## What is owed

The after-measure — the mini's ready/in-flight counts, open PRs per
repo, and the oldest ready item's age, taken once items 1 and 2 have
merged to `main` and the mini has updated past `v0.2.192` — is owed,
and belongs in a later item of this goal or a follow-up goal doc. In
particular: whether `no-pr-outlives-its-goal`'s long-ready item (still
waiting at measurement time) is claimed sooner once it carries started
work under the new order, and whether `runsheet/runsheet`'s open pull
requests stay at or below its merge-queue cap once the per-repo limit
is live on the mini.

Also owed: this document's comparison against `main`. The branch is
stacked on item 2's (`mesh/finish-before-you-start-2`, itself on item
1's), and ADR-017 reaches `main` only with `#458` and `#465`. Until
both merge, `git diff origin/mesh/finish-before-you-start-2 --stat`
shows this file alone and the claim order above matches ADR-017 on
that branch. Against `origin/main` the diff still carries items 1 and
2's commits, and ADR-017 is not there. Once they merge, this branch is
rebased onto `origin/main`. `git diff origin/main --stat` then shows
this file alone, and the claim order is checked again against ADR-017
on `main`. Rechecked at 2026-09-29T16:12Z: both pull requests are
still open, and the mini still runs v0.2.191 (latest release v0.2.193).
