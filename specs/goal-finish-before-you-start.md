---
title: Goal — Finish before you start: an item with an open pull request is claimed before a fresh one, and a repo has a limit on unmerged work
kind: plan
status: running
parent: prd
summary: ADR-017 records the claim order in full — finish-first bounded by two failed evaluations, then oldest readyAt, then seq — and node.yaml's claim.unmerged_per_repo holds fresh claims for a repo with that many items in review; the mini has not yet run a release carrying either change, so the after-measure is still owed.
---

# Goal — Finish before you start: an item with an open pull request is claimed before a fresh one, and a repo has a limit on unmerged work

Mesh goal `finish-before-you-start`, running: items 1, 2 and 4 merged, item 3
failed three times and was superseded, this document is item 5.

## Statement

The node preferred starting new work to finishing started work, and did so
silently. `claimOrder` (`internal/node/loop.go`) sorted ready items by the
goal's priority, then fresh items before retries, then seq — so at equal
priority an item already built, reviewed and sent back queued behind every
brand-new item, and work in progress accumulated.

Measured on 2026-09-29 (the baseline): **22 items ready against 5 in
flight**; `runsheet/runsheet` at **13 open pull requests** against a
merge-queue cap of 7, with two items cancelled outright for `merge queue
full: 15 of 7`; and item #1075 of `no-pr-outlives-its-goal` `ready` since
2026-09-28T02:24Z — **35 hours** — with a rebase conflict recorded in full
and newer items claimed ahead of it the whole time.

The goal's fix, in the order it was worth doing: finish before you start (an
item with started work goes before a fresh one at equal priority, bounded so
a poisoned item cannot starve the queue), a limit on unmerged work per repo
(node.yaml, not a constant), and age as a tiebreak within a class.

## Acceptance (the goal)

`go vet ./...` clean, `gofmt -l .` empty, `go build ./cmd/mesh/` and the
linux/amd64 cross-build fine, `go test ./...` green.

`claimOrder` tests assert the whole claimed order, not a pairwise predicate:
started work before fresh at equal priority; priority still outranks the
class; an item at or past its attempt cap loses the preference; the older
`readyAt` goes first within a class; the sort is stable. The bound against
starvation is proved with a poisoned item that would otherwise cycle
forever, showing the queue still makes progress elsewhere, and the ADR names
the bound and why. The per-repo limit holds no new claim for a repo at the
limit but still claims that repo's rebases and rework, is read from
node.yaml, re-reads live, and its absence keeps prior behaviour.
`evaluateOrder` is either changed with the same argument and tests, or left
alone with the reason recorded here. A decision on `reviewPile` and
`mergePressureAt` is recorded here with why. An ADR records the claim order
as it now stands, in full. The goal doc carries evidence from the mini after
the change — ready against in flight, open pull requests per repo, and the
oldest `readyAt` — against the baseline, naming the number that moved.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | Node: finish before you start — at equal priority an item with started work is claimed before a fresh one, bounded by attempts, oldest readyAt first; ADR-017 records the claim order in full | merged | mesh/finish-before-you-start-1 | [runsheet/mesh-cli#458](https://github.com/runsheet/mesh-cli/pull/458) | $3.27 |
| 2 | Node: a limit on unmerged work per repo — node.yaml `claim.unmerged_per_repo`, read live, holds fresh claims for a repo at the limit but never the rework of an item already there; ADR-017 amended | merged | mesh/finish-before-you-start-2 | [runsheet/mesh-cli#465](https://github.com/runsheet/mesh-cli/pull/465) | $4.60 |
| 3 | Goal doc: finish-before-you-start — the claim order as it now stands, the evaluateOrder decision, and the mini's ready/in-flight, open PRs per repo and oldest readyAt against the 2026-09-29 baseline | **failed** | mesh/finish-before-you-start-3 | — | $2.77 |
| 4 | Node: a limit on unmerged work per repo — node.yaml `claim.unmerged_per_repo`, read live, holds fresh claims for a repo at the limit but never the rework of an item already there; ADR-017's per-repo section filled in (PR #465 carried onto main) | merged | mesh/finish-before-you-start-4 | [runsheet/mesh-cli#473](https://github.com/runsheet/mesh-cli/pull/473) | $2.94 |
| 5 | Goal doc: finish-before-you-start — the claim order as it now stands, the evaluateOrder and reviewPile decisions, and the mini's ready/in-flight, open PRs per repo and oldest readyAt against the 2026-09-29 baseline, labelled by the node version that produced them | running (this document) | mesh/finish-before-you-start-5 | — | — |

**Why item 3 failed.** It was planned and built while item 2 was still on its
own unmerged branch, not on `main`: item 3's branch diffed from `origin/main`
by 39 files, because `main` did not yet carry ADR-017 or item 2's per-repo
limit. All three evaluations (attempts 1–3, on Codex gpt-6-sol) found the
document itself correct — frontmatter, sections, claim order and
`evaluateOrder` explanation all matched what item 3's own branch carried —
but failed it on the literal acceptance: `git diff origin/main --stat` had
to show the goal document alone, and it did not. Each evaluation asked for a
retry after items 1 and 2 merged; the item was superseded by item 4 (which
carried item 2's change onto `main`) and this item (5) instead of being
retried directly. This document is based on `main` after item 4 merged, and
changes one file.

## The claim order as it now stands

[ADR-017](../docs/adr/adr-017-finish-before-you-start.md) records this in
full; the summary below matches it and does not contradict it.

The node claims ready items from every org it serves in seven steps.
**Priority** comes first: urgent, high, normal, low, and an unknown or
unreadable priority is normal; priority outranks every step after it, so an
urgent fresh item is claimed before a normal retry. At equal priority,
**finish first**: an item that carries started work — an open pull request,
a retry after a failed evaluation, a rebase conflict, or a rebase requested,
claimed or unresolved — goes before every item that does not, as long as it
is under **the attempt bound**. That bound is `finishFirstAttempts = 2`: the
platform fails an item at three failed evaluations, so the node's bound is
one under the platform's cap, giving a poisoned item at most two turns at
the head of its priority's queue before it falls back behind fresh work and
the platform fails it on the third. A bound of three would never bind (the
item is failed first); a bound of one would give an item sent back once for
rework no preference at all, which is the case the goal exists for. Among
items not in the finish-first class, **fresh items go before bounded-out
retries**, as before. Within a class, **the item ready longest goes
first** — the platform's `readyAt`, parsed and compared as a time, not a
string; an item with no `readyAt` or one that does not parse sorts as if it
became ready now, and among such items by seq (the key is
`(has a time, the time, seq)`, chosen because comparing only when both
sides carry a time is not itself an order). Ties then fall to **seq**, and
the whole sort is **stable**, so items tied on every step keep the order the
orgs and their items were listed in.

**The per-repo limit** sits beside the claim order, not inside it: node.yaml
`claim: {unmerged_per_repo: N}` (`ClaimConfig`, `Loop.UnmergedPerRepo`, read
live on `Loop.reload` like `context:`) holds a fresh claim for a repo that
already has N items in `review` — counted from the node's own listing across
every served org, never `RepoIndex.reviewPile` — while a rebase, rework or
re-evaluation already in that repo is still claimed regardless of the count,
because it finishes a pull request that already counts rather than opening a
new one. Its default (absent or 0) is no limit and no `review` read at all,
so a node that does not set it behaves exactly as before.

## evaluateOrder and rebaseOrder

Left alone, for the three reasons ADR-017 gives: every item this node holds
for evaluation or rebase is this node's own started work already, so the
finish-first class does not tell them apart — each one *is* started work;
evaluations are not capacity-gated, so there is no queue of fresh starts that
could displace them by running whenever an account has headroom instead of
waiting a turn; and the item carries no time it entered `evaluating`, so
there is no age to break ties on the way `readyAt` does for claims.
`evaluateOrder` keeps priority then seq; `rebaseOrder` is left the same way,
since every item it orders is started work by definition.

## reviewPile and mergePressureAt

Not read. ADR-017's "Per-repo limit on unmerged work" section gives the
reason: the node's own `review` listing is exact for what the integrator is
actually about to merge, needs no new gateway read or field (`OrgWorkItems`
and `repoId` are already selected), and does not tie the limit to a
platform-side definition (`reviewPile`'s meaning, `mergePressureAt`'s
trigger) that is undocumented in this repo and could change under it. If the
platform documents them and the node later needs a signal it cannot derive
from `review`, that section is where to revisit it — not here.

## Live check on the mini

| Row | at | mini version | desired_version | commits behind main | ready | in flight | open PRs — runsheet/runsheet | open PRs — mesh-cli | oldest ready item | waiting |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Baseline (ADR-017) | 2026-09-29 | — | — | — | 22 | 5 | 13 | — | `no-pr-outlives-its-goal` item #1075 | 35 hours |
| Measured (this item) | 2026-09-29T17:35Z | v0.2.191 | v0.2.191 | 9 | 8 | 5 | 3 | 1 | `no-pr-outlives-its-goal` seq 2, ready_at 2026-09-28T02:24:04Z | 2351 min (~39.2 h) |

The measured row is `mesh_health_nodes`, `mesh_health_flow` and
`mesh_health_stuck` for org `runsheet`, all read within the same minute.
Ready and in flight are the sum of `mesh_health_flow`'s per-project figures
(`runsheet/loom`: 0 ready, 0 running; `runsheet/mesh`: 3 ready, 1 running;
`runsheet/runsheet`: 5 ready, 4 running); open pull requests per repo are
`mesh_health_stuck`'s `review` items counted by the repository their pull
request belongs to (3 on `runsheet/runsheet`, 1 on `runsheet/mesh-cli`, none
elsewhere); the oldest ready item is `runsheet/runsheet`'s
`oldest_runnable` — the same item ADR-017 named, at the same `ready_at`, now
waited on longer.

**The mini does not carry items 1 or 4.** `mesh_health_nodes` reports the
mini at `version v0.2.191` (tagged 2026-09-29T13:56:22Z, commit `8eba662`)
against `main_head bae3229` (item 4's merge, PR #473, 2026-09-29T17:10:07Z)
9 commits behind — which also postdates item 1 (PR #458,
2026-09-29T16:15:45Z). So this row is a **second baseline, not an
after-measure**: the after-measure is owed until the mini runs a release
carrying both #458 and #473. The goal expects two numbers to move once it
does — the oldest `readyAt` in the ready queue, and the open pull requests
per repo — and neither can be credited to this goal from the numbers above:
`runsheet/runsheet`'s open pull requests are lower than the baseline's 13
(now 3) and its oldest ready item is now older, not younger (39.2 hours
against 35), but both reflect the platform's own state on an unrelated
version, not this goal's claim order or per-repo limit taking effect.

## Still owed

- **The mini's upgrade.** `desired_version` (v0.2.191) equals the running
  version — no pin is forcing an upgrade, so self-update does nothing on its
  own — and the mini is 9 commits behind `main`. It needs a release built
  from a commit at or after item 4 (`bae3229`), by the release path or by
  hand (AGENTS.md, "Deploying to the mini by hand"), before either change in
  this goal takes effect there.
- **The after-measure.** Once the mini runs that release, re-read
  `mesh_health_nodes`, `mesh_health_flow` and `mesh_health_stuck` for org
  `runsheet` and record a third row here, naming which of the two numbers
  moved — the oldest `readyAt` in the ready queue, and the open pull
  requests per repo, particularly `runsheet/runsheet`'s.

Nothing else is owed: `evaluateOrder` and `rebaseOrder` are left alone with
the reason recorded above, and `reviewPile`/`mergePressureAt` are a decision
recorded above, not a gap.

## Log

- 2026-09-29 · item 1 (#458) merges: `claimOrder`'s finish-first order,
  bounded by two failed evaluations, oldest `readyAt` first; ADR-017 written.
- 2026-09-29 · item 2 (#465) merges: `claim.unmerged_per_repo`, ADR-017
  amended with the per-repo section.
- 2026-09-29 · item 3 (this document, first attempt) fails three times: its
  branch was based on item 2's still-unmerged branch, so its diff from
  `origin/main` carried 39 files and `origin/main` did not yet carry
  ADR-017; each evaluation asked for a retry after items 1 and 2 merged.
- 2026-09-29 · item 4 (#473) merges: item 2's per-repo limit carried onto
  `main`, ADR-017's per-repo section now on `main` in full.
- 2026-09-29 · this document (item 5), based on `main` after item 4. Live
  check shows the mini still on v0.2.191, 9 commits behind `main` and
  missing items 1 and 4: the row above is a second baseline, and the
  after-measure stays owed until the mini upgrades.
