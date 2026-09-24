---
title: Goal — A node updates itself to the version the platform asks for
kind: plan
status: draft
parent: prd
---

# Goal — A node updates itself to the version the platform asks for

Mesh goal `node-self-update`, running.

## Statement

Chris, 2026-09-24: nodes should be updatable from the console. Today a node update is a hand job: build mesh-cli for linux/amd64, scp to the mini as ~/bin/mesh.new, wait for no claude session, swap, restart the systemd unit. On 2026-09-23 it was done three times, once killing sessions mid-flight because the node is never idle, and on 2026-09-24 it could not be done at all while the laptop was off the mini's network. The node already reports its version on heartbeat (NodeIndex.version). Node half (the platform and console half is goal update-nodes-from-console on the runsheet project; the drain is goal node-drains: build on it, and if it has not landed, this goal's first item is the drain). (1) Releases: a GitHub Actions workflow on projectmesh-io/mesh-cli that, on a tag, builds linux/amd64, linux/arm64 and darwin/arm64 binaries with the version stamped (internal/version) and publishes them as a GitHub release with a SHA256SUMS file signed by keyless Sigstore (cosign sign-blob, the workflow's GitHub OIDC identity). (2) Self-update: on each heartbeat the node reads its desired version (a NodeIndex field the platform half adds; empty means leave it be). When it differs from its own, the node drains, downloads that release's binary for its OS/arch from GitHub, verifies the checksum and the Sigstore signature against the workflow identity (refusing anything else and reporting why), atomically replaces its own executable (keeping the previous as <path>.prev), and exits 0 for systemd or launchd to restart it; after restart it reports the new version. A failed verification, or a new binary that fails to start twice, rolls back to .prev and reports the failure on the heartbeat. `mesh node update [version]` does the same by hand.

## Acceptance

Tests: a desired version different from the running one drains then swaps; a checksum or signature mismatch refuses and reports; a binary that fails to start rolls back to .prev; an empty desired version does nothing. The release workflow runs on a test tag and produces signed artifacts that `mesh node update` verifies. README documents releasing and the update path.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | Node drain: stop claiming, wait for running sessions, report draining | running | — | — | $0.88 |
| 2 | Tag-triggered release workflow with keyless Sigstore-signed SHA256SUMS | review | mesh/node-self-update-2 | https://github.com/projectmesh-io/mesh-cli/pull/67 | $0.64 |
| 3 | internal/selfupdate: fetch, verify (checksum + Sigstore), atomic swap, rollback state | pending | — | — | $0.00 |
| 4 | Wire self-update into the node loop: desired version, drain, apply, restart, rollback | pending | — | — | $0.00 |
| 5 | `mesh node update [version]` and the release workflow's verify job | pending | — | — | $0.00 |
| 6 | ADR 003: node self-update — trust root, drain, rollback; TRD amendment | pending | — | — | $0.00 |

## Log

- 2026-09-24 · item 1 · session started
- 2026-09-24 · item 1 · The drain is built and committed on `mesh/node-self-update-1` as `3cddfa4`. `go vet ./... && go build ./cmd/mesh/ && go test ./...` passes, and the new tests pass three times over under `-race`. **Had node-drains already landed?** No, apart from one piece. Prologue shows only its live-config-reload item merged (#60) and its TRD §3 "Drain" text merged in mesh-specs. The drain itself (item 8) is still `ready`, and main has no drain code. So I built it here. The PR can say it adds the whole drain on top of #60's reload, with nothing duplicated. **What changed** - **The loop** (`internal/node/loop.go`) gains three methods: - `Drain(ctx)` turns draining on. The tick keeps sending heartbeats and keeps evaluating items this node finished, but it no longer claims items or starts planning or research. `Drain` returns `nil` once the node is idle, or `ctx.Err()` if the context ends first. - `Draining()` reports whether the node is draining. - `Resume()` turns draining off, so the next tick claims again. - **The heartbeat** (`internal/gateway/client.go`): `HeartbeatNode(..., extras ...HeartbeatExtras)` takes an optional `HeartbeatExtras{Draining, UpdateError}`. Each field goes into the mutation only when it is set. Because the argument is optional, existing callers compile unchanged. The loop sends `Draining` on every tick. - **AGENTS.md**: a paragraph under the node bullet says a draining node claims nothing and that the extra heartbeat fields are sent only when set. **One departure from the brief:** `Drain` waits until every kind of session has ended (builds, evaluations, plans and research), not just until `Running()==0`. TRD §3 Drain requires this, and it points out that waiting only for the builds that count against capacity would let the restart kill a running evaluator or planner. Test (b) passes either way. **Tests** (added to `internal/node/loop_test.go`, against the fake gateway): - **(a)** A draining tick claims no item and no plan, still evaluates the held item, and its heartbeats carry `draining: true`. - **(b)** `Drain` stays blocked for 200ms while an item runs, then returns `nil` once the item ends. - **(c)** Cancelling the context makes `Drain` return `context.Canceled` while the item keeps running. - **(d)** After `Resume`, the next tick claims the ready item and the heartbeat no longer reports draining. - **(e)** The heartbeat payload has no `draining` or `updateError` key when no extras are passed or they are empty. It has `draining: true` or `updateError` only when that field is set. **Design docs:** nothing was amended on this branch. The drain is already described in TRD §3, which lives in mesh-specs outside this workspace. The new heartbeat fields are the platform's contract, owned by runsheet's `update-nodes-from-console`. That platform half needs to land before a node starts sending `draining` to it. As AGENTS.md requires, the commit has no Co-Authored-By trailer. Nothing was pushed.
