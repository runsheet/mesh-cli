---
title: Goal — A node takes a rebase the integrator could not do itself
kind: plan
status: draft
parent: prd
---

# Goal — A node takes a rebase the integrator could not do itself

Mesh goal `node-runs-rebase-sessions`, running.

## Statement

2026-09-24: goal merge-queue made the integrator rebase a conflicting PR in-process when the conflict is mechanical, and otherwise RequestWorkItemRebase — asking a node for a session of kind rebase (ADR 008, trd §6a; ClaimWorkItemRebase / FinishWorkItemRebase / FailWorkItemRebase). mesh-cli has no rebase session, so six PRs (runsheet#306, #310, #325, console#76, #82, website#5) sat conflicting with rebase_state requested and nothing moving, until FailWorkItemRebase was sent by hand through /ops, handing each conflict to its builder. Node half: the node lists items with rebase_state requested in the orgs it serves (claimed in priority order with the rest of its work, see node-claims-by-priority), claims one with ClaimWorkItemRebase, runs a session whose whole job is: check out the item's branch, rebase onto the base, resolve the conflicts keeping both sides' intent (the mesh-branch-rebase skill in runsheet/runsheet is the runbook), run the item's acceptance checks, push with --force-with-lease, and FinishWorkItemRebase with the new head and the files it resolved — or FailWorkItemRebase with why, which hands the item to its builder. It does not count against capacity the way an evaluation does not. The gateway already serves the commands and the rebase_state field.

## Acceptance

Tests with the fake gateway: a requested rebase is claimed once, runs a rebase session, and finishes with the pushed head; a session that cannot resolve fails the rebase with its reason; two nodes never both claim one; rebase sessions follow the priority order. README documents the kind.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | Gateway client and fake: rebase_state, claim, finish, fail | running | — | — | $1.26 |
| 2 | The rebase session: prompt, runner, loop wiring, priority order | pending | — | — | $0.00 |
| 3 | Document the rebase session kind | pending | — | — | $0.00 |

## Log

- 2026-09-24 · item 1 · session started
- 2026-09-24 · item 1 · The node's gateway client can now see, claim, finish and fail a rebase. `go vet ./... && go build ./cmd/mesh/ && go test ./...` all pass. It's committed as `6900c28` on `mesh/node-runs-rebase-sessions-1` and not pushed. **Where the names come from:** runsheet's ADR 008 (step 2), TRD §6a ("A conflict is not a rework", the bullet "Else a node session of kind rebase"), and the command definitions in runsheet's `platform/schema/platform.loom`. I read the schema from a local runsheet checkout the node had already made (read only). **Three places I followed the platform instead of the brief:** - **No cost on finish or fail.** The platform's `FinishWorkItemRebase` takes only `head_sha`, `method` and `files`, and `FailWorkItemRebase` takes only `reason`. Sending a `costCents` field would make the platform reject the call. So the two methods are `FinishWorkItemRebase(ctx, itemID, headSha, resolvedFiles)` and `FailWorkItemRebase(ctx, itemID, reason)`. Item 2's session should report its cost with `ReportWorkItemProgress`, which is also what keeps a claimed rebase alive. - **The state names are the platform's.** A rebase moves from `requested` to `claimed`, then to `rebased` or `unresolved`, not "done" or "failed". Finish always sends `method: session`. - **A second claim by the same node succeeds.** The platform treats that as a repeat; only a different node, or a rebase nobody requested, is refused. **What changed:** - **`internal/gateway/client.go`:** - `WorkItem` gains `RebaseState` (json `rebaseState`) plus the conflict text, its files, the node holding the rebase and the rebased head; all are also in `workItemFields`. - `RequestedRebases(ctx, orgID)` lists an org's items with `rebase_state` `requested`, in seq order. - `ClaimWorkItemRebase`, `FinishWorkItemRebase` and `FailWorkItemRebase` all go through `c.Mutate`. - **`internal/gateway/gatewaytest/fake.go`:** - A claim moves `requested` to `claimed` for one node. - Finish records the head and files and sets `rebased`, leaving the item in review with its attempts. - Fail records the reason and hands the item back to its builder, as ADR 008 step 4 does: status `ready`, the reason as its last error, attempts unchanged, rebase state back to `none`. Failing again does nothing. - **`internal/gateway/rebase_test.go`:** tests that: - the listing returns only this org's requested items; - a second node's claim gets a gateway error while the holder's second claim succeeds; - a claim on a rebase nobody requested is refused; - finish records the head and files; - fail records the reason and hands the item back. I didn't change any ADR or TRD, because this item only uses commands the platform already serves. The README entry for the new kind is item 3.
