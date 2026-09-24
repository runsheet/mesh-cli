---
title: Goal — A node that cannot pay for a session stops, instead of failing items
kind: plan
status: draft
parent: prd
---

# Goal — A node that cannot pay for a session stops, instead of failing items

Mesh goal `node-out-of-credit`, running.

## Statement

2026-09-23 18:10–21:00Z: the mini's Claude account hit its monthly spend limit, and for three hours every session ended at once with "You've hit your monthly spend limit · raise it at claude.ai/settings/usage" — 8,813 times in node.log. Each was treated as the item's own failure: 8 items failed across 5 goals, each goal spent a re-plan, one planner was retried every few seconds for three hours, and nobody was told. Detect the harness's out-of-credit and rate-limit messages (and a non-zero exit with no output twice running) as the node's condition, not the item's: release the claim without counting an attempt, stop claiming new work, report it on the node's heartbeat (a harness_unavailable status and reason, which the platform turns into one needs_person row — coordinate the field with the platform), and probe once every ten minutes with a minimal session, resuming on success.

## Acceptance

Tests: a session ending with the spend-limit message releases its item with attempts unchanged and puts the node in harness_unavailable; no new claims while in it; a successful probe resumes claiming; the heartbeat carries the reason. README: what an operator sees and does.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | Spec: the node's harness-unavailable condition — ReleaseWorkItem, HeartbeatNode status/reason, detection and the probe | merged | mesh/node-out-of-credit-1 | https://github.com/projectmesh-io/mesh-specs/pull/9 | $0.56 |
| 2 | harness: classify a session's exit as out-of-credit/rate-limited or silently failed | merged | mesh/node-out-of-credit-2 | https://github.com/projectmesh-io/mesh-cli/pull/63 | $0.78 |
| 3 | gateway: ReleaseWorkItem and a HeartbeatNode that carries status and reason | claimed | mesh/node-out-of-credit-3 | https://github.com/projectmesh-io/mesh-cli/pull/61 | $0.63 |
| 4 | node: stop on harness-unavailable — release, gate claiming, heartbeat the reason, probe and resume | pending | — | — | $0.00 |

## Log

- 2026-09-24 · item 3 · session started
- 2026-09-24 · item 3 · Item 3 is done and committed on `mesh/node-out-of-credit-3` as `9605e40`. The acceptance check passes: `go vet ./...` and `go build ./...` are clean, and `go test ./...` passes everywhere, with `-race` too for `internal/gateway` and `internal/node`. **What changed** - **`internal/gateway/client.go`**: - `ReleaseWorkItem(ctx, itemID, reason)` is new and sits next to `FailWorkItem`. It sends the `releaseWorkItem` mutation with `{reason}`. - `HeartbeatNode` now also takes `status` and `reason` and sends both in the mutation. - **`internal/node/workspace.go`**: the `Gateway` interface has the new `HeartbeatNode` signature and now lists `ReleaseWorkItem`. - **Callers**: `internal/node/loop.go` and `internal/commands/nodecmd.go` (`mesh node status`) pass `"", ""`, so they behave as before. Seq 4 wires the loop's real values. - **`gatewaytest/fake.go`**: - A new `releaseWorkItem` case sets the item back to `ready`, puts the reason in `lastNote`, and removes `nodeId` and `harness`. It leaves attempts and `lastError` alone. - The `heartbeatNode` case now saves `status` and `reason` on `f.Nodes[id]`. **Two things I did beyond the brief** - The fake's release also clears `sessionId`, because the TRD says a release clears `session_id`. - When a heartbeat's status is empty, the fake stores `online` rather than an empty string. That matches the TRD, where an empty status means the ordinary heartbeat. Without it, a normal heartbeat would wipe the `online` status set at registration. **Tests** (`internal/gateway/release_test.go`) - `TestReleaseWorkItem`: starts from a claimed item, releases it, and checks that the status is `ready`, `nodeId` and `harness` are cleared, `lastError` and `attempts` are unchanged, and `lastNote` is the reason. - `TestHeartbeatNodeStatus`: after a `harness_unavailable` heartbeat with the spend-limit message, the node row shows that status and reason. A later ordinary heartbeat sets it back to `online` with an empty reason. **Design docs:** no change needed. TRD §1 already describes `ReleaseWorkItem { reason }` and `HeartbeatNode { version, running, status, reason }` exactly as built. `go` isn't on this session's `PATH`; I ran it from `/usr/local/go/bin`.
- 2026-09-24 · item 3 · session started
