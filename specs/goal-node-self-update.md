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
| 1 | Node drain: stop claiming, wait for running sessions, report draining | claimed | — | — | $0.00 |
| 2 | Tag-triggered release workflow with keyless Sigstore-signed SHA256SUMS | review | mesh/node-self-update-2 | https://github.com/projectmesh-io/mesh-cli/pull/67 | $0.64 |
| 3 | internal/selfupdate: fetch, verify (checksum + Sigstore), atomic swap, rollback state | pending | — | — | $0.00 |
| 4 | Wire self-update into the node loop: desired version, drain, apply, restart, rollback | pending | — | — | $0.00 |
| 5 | `mesh node update [version]` and the release workflow's verify job | pending | — | — | $0.00 |
| 6 | ADR 003: node self-update — trust root, drain, rollback; TRD amendment | pending | — | — | $0.00 |

## Log

- 2026-09-24 · item 1 · session started
