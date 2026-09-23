---
title: Goal — Directions on the node: a research session belongs to a direction
kind: plan
status: draft
parent: prd
---

# Goal — Directions on the node: a research session belongs to a direction

Mesh goal `directions-on-the-node`, running.

## Statement

On 2026-09-23 the platform finished moving a project's director onto a `Direction` aggregate of its own — several per project, each with its own words, window, timezone, gap, ceiling and research claim. `DirectionIndex`, `ClaimDirectionResearch` and `RecordDirectionResearch` are all built, deployed and proven.

The node was never ported. `gateway.Project` still carries `Direction`, `DirectorEnabled` and `MaxLiveGoals` as `json:"-"` stubs that are always zero, so `researchDue` skips every project silently, and `TestLoopResearchesDirectedProjects` and `TestLoopSkipsAFullProject` are skipped with "the node's director is being ported to DirectionIndex".

The consequence is that **no project researches anything, anywhere** — wardroom's director has been dead since the deploy, and a direction written on any project is words nothing reads. This goal is what makes directions real.

## Acceptance

A node with a direction enabled on a project inside its window claims that direction, runs one research session on its words and hands the proposals in against the direction. Two directions on one project research independently on their own hours. The two skipped loop tests run and pass. Proven with a fake gateway in the node's own tests, and by wardroom's director producing a proposal again after the node is upgraded.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | The node lists directions and claims one, not a project | merged | mesh/directions-on-the-node-1 | https://github.com/projectmesh-io/mesh-cli/pull/6 | $5.79 |
| 2 | A research session belongs to a direction, and two on one project run on their own hours | claimed | — | — | $0.00 |

## Log

- 2026-09-23 · item 1 · session started
- 2026-09-23 · item 1 · Item 1 is done. Two commits on `mesh/directions-on-the-node-1`. ## What changed **`internal/gateway/client.go`** - `Direction` mirrors a `DirectionIndex` row — id, projectId, orgId, slug, name, direction, enabled, fromMinute, toMinute, timezone, gapMinutes, maxLiveGoals, status, researchNodeId, researchClaimedAt, researchedAt. - `Directions(ctx, orgID, projectID)` lists them. The `org_id` where is always there because the gateway scopes the index on it; `project_id` narrows when given (`""` means the whole org, which is how the loop reads one page for every project). `Direction.Active()` is the only judgement the node makes itself: on, and not archived. - `ClaimDirectionResearch(ctx, directionID, nodeID)` and `RecordDirectionResearch(ctx, directionID, nodeID, proposals, costCents, notes)` replace `ClaimResearch`/`RecordResearch`, mutating the direction's aggregate id. Refusal handling is untouched — the window, the gap, the claim and whether the direction is on stay the platform's, so `IsRefusal` is still the ordinary answer. - `Direction`, `DirectorEnabled`, `MaxLiveGoals` and their comment are gone from `Project`. **The callers had to move with it**, or the build could not pass: `Loop.Research` and `Director.Research` now take a `gateway.Direction` alongside the project, `researchDue` lists directions and keys its in-flight set by direction id (so two directions of one project don't block each other), `RoomFor` takes the direction's ceiling, and `researchWith` in `nodecmd.go` follows. `gatewaytest.Fake` grew a `Directions` table and the two commands. I left the research workspace at `<root>/<project>/research` — splitting it per direction belongs with item 2's "two on one project run on their own hours", along with counting room per direction (`GoalIndex.directionId`) rather than per project. ## Acceptance `go vet ./... && go build ./... && go test -count=1 ./...` — clean, every package `ok`. `grep` finds no `DirectorEnabled`, `ClaimResearch` or `RecordResearch` anywhere in the repo. New tests, all on the existing fake gateway: `TestDirections` (a project's directions listed and every field decoded, `Active()` both ways), `TestDirectionResearchClaimAndRecord` (claim and hand-in against the direction id, the cost and proposals, the claim let go, and a refused claim still `IsRefusal`), and `TestLoopKeepsQuietOnARefusedDirection` (a refusal runs no session, logs nothing, and the direction is offered again next tick). `TestLoopResearchesDirectedProjects` and `TestLoopSkipsAFullProject` no longer skip — both run and pass on directions, the first now also checking that a direction switched off and an archived one are left alone. Docs: AGENTS.md's director bullet and the README's director section now describe directions; no ADR or TRD change is due here — the aggregate and its policy are runsheet's, already recorded in its trd §6c on 2026-09-23, and this repo is the consumer.
- 2026-09-23 · item 2 · session started
