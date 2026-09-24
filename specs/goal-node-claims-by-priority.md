---
title: Goal — The node claims urgent work first
kind: plan
status: draft
parent: prd
---

# Goal — The node claims urgent work first

Mesh goal `node-claims-by-priority`, running.

## Statement

Goals now carry a priority (urgent, high, normal, low; empty reads as normal) — runsheet's merge-queue goal added GoalIndex.priority, and the integrator merges each repo's PRs in that order. But the node still claims ready work oldest seq first (internal/gateway/client.go ReadyWorkItems → sortBySeq; internal/node/loop.go walks the orgs in turn), so an urgent goal's item waits behind a low one's. On 2026-09-24 Chris set priorities on all 28 live goals across runsheet, projectmesh-io, go-apis and wardroom. Claim in priority order: collect ready items across every org the node serves, join each to its goal's priority (one GoalIndex read per org per tick, not per item), and claim urgent first, then high, normal, low; within a priority, oldest evaluation-free item first, then seq. Planning and evaluation are claimed the same way (a planner for an urgent goal first). No starvation guard beyond that for now: note it in the README. The platform already serves the field; this is node-only.

## Acceptance

Tests with the fake gateway: with capacity 1, a ready item of an urgent goal in one org is claimed before an older ready item of a normal goal in another org; empty priority sorts as normal; plans and evaluations follow the same order; one GoalIndex read per org per tick. README documents the order.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | Claim ready work in priority order across orgs | merged | mesh/node-claims-by-priority-1 | https://github.com/projectmesh-io/mesh-cli/pull/45 | $1.15 |
| 2 | Order planning and evaluation by goal priority; document the order | failed | — | — | $0.00 |
| 3 | Order planning and evaluation by goal priority; document the order | claimed | — | — | $0.00 |

## Log

- 2026-09-24 · item 1 · session started
- 2026-09-24 · item 1 · The node now claims ready work in priority order across all the orgs it serves. `go vet ./... && go build ./... && go test ./...` passes in mesh-cli, and everything is committed on `mesh/node-claims-by-priority-1` as `a0854ce`. **What changed** - **Gateway** (`internal/gateway/client.go`): goals now read `priority`, and a new `PriorityRank` ranks urgent, high, normal, low in that order. An empty or unknown priority counts as normal. - **Loop** (`internal/node/loop.go`): each tick lists every org's ready items and reads that org's goals to get each item's priority. It then sorts them all into one list with a new function, `claimOrder`: priority first, then items with no evaluation before retries, then seq. Ties keep the order the orgs are listed in. It claims down that list until capacity runs out; running items and refused claims are handled as before. - **Goal reads**: each org's goals are read once per tick, and the planner and director steps reuse that read. Before, each of them read the goals separately. An org with no ready items isn't read just for claiming. If the read fails, the error is logged and that org's items count as normal. - **Fake gateway**: it now records the lists it serves (`ReadsNamed`) and can be told to fail one (`FailReads`), so tests can count goal reads per org. - **README**: now states the claim order and that there is no starvation guard. That note is part of the goal, not this item's brief. **Tests** (`internal/node/loop_test.go`, plus a `PriorityRank` test) - With capacity 1, an urgent item in globex is claimed before an older normal item in acme, even though acme is listed first. - Empty priority ties with normal and breaks on seq; low comes after it. - The full order: urgent, then high, then normal, then low. Within high, fresh items come before the retry, then seq. - Goals are read once per org per tick with several ready items, and not at all for an org with nothing ready. - When the goal read fails, that org's items are still claimed, as normal. - A direct test of `claimOrder`. **Not done here** - Planning and evaluation still aren't claimed in priority order. The goal asks for that, but this item's brief doesn't. - I didn't amend a TRD or ADR. The claim order is described in the TRD in `mesh-specs`, which is outside this workspace. The platform already serves the `priority` field, so it doesn't need a change first.
- 2026-09-24 · item 3 · session started
