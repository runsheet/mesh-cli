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
| 1 | Node drain: stop claiming, wait for running sessions, report draining | pending | — | — | $0.00 |
| 2 | Tag-triggered release workflow with keyless Sigstore-signed SHA256SUMS | running | — | — | $0.39 |
| 3 | internal/selfupdate: fetch, verify (checksum + Sigstore), atomic swap, rollback state | pending | — | — | $0.00 |
| 4 | Wire self-update into the node loop: desired version, drain, apply, restart, rollback | pending | — | — | $0.00 |
| 5 | `mesh node update [version]` and the release workflow's verify job | pending | — | — | $0.00 |
| 6 | ADR 003: node self-update — trust root, drain, rollback; TRD amendment | pending | — | — | $0.00 |

## Log

- 2026-09-24 · item 2 · session started
- 2026-09-24 · item 2 · The tag-triggered release workflow is in place and passes the item's checks, but it has not run on GitHub yet — that happens on the first pushed tag. Everything is committed on `mesh/node-self-update-2` as `01b4ffe`. **What I added** - **`.github/workflows/release-tag.yml`** - Runs only when a tag starting with `v` is pushed, so test tags like `v0.0.0-test.1` count too. There is no manual trigger. - Permissions are `contents: write` and `id-token: write`. - The build job makes three binaries with `CGO_ENABLED=0 -trimpath` and the tag stamped as `internal/version.Version`: `mesh-linux-amd64`, `mesh-linux-arm64` and `mesh-darwin-arm64`. - The publish job: - writes `SHA256SUMS`; - installs cosign with `sigstore/cosign-installer@v3`; - signs keyless with `cosign sign-blob --yes --bundle SHA256SUMS.sigstore.json SHA256SUMS`; - runs `gh release create` on the repo itself, attaching the three binaries, `SHA256SUMS` and `SHA256SUMS.sigstore.json`. A tag with a `-` in it becomes a prerelease. - Two steps the brief didn't ask for: a check that all three binaries arrived before signing, and a `cosign verify-blob` against the exact certificate identity and issuer before publishing. A wrong identity therefore fails the workflow instead of producing a release that nodes would refuse. - **README `## Release`:** rewritten to cover how to cut a release, the five asset names, the signing identity and issuer, and a by-hand check (`cosign verify-blob …` then `sha256sum --check`). It also notes that `cli-release.yml` (pushes to `main`, released to the mesh repo) and the `release.yml` stub are not what nodes update from. - **`docs/adr/adr-002-signed-tag-releases.md`:** a new ADR, because the asset names and signing identity are a public contract the updater will depend on. It also records two consequences: whoever can push `v*` tags can ship to nodes, and darwin/amd64 and Windows nodes cannot update themselves. **How the acceptance was checked** - **actionlint:** it wasn't installed, so I ran it with `go run github.com/rhysd/actionlint/cmd/actionlint@latest`; it passed. `yaml.safe_load` also shows the `tags: ['v*']` trigger, both permissions and the three exact asset names. - **Old workflows:** `git diff main` shows `release.yml` and `cli-release.yml` unchanged. The goal's end-to-end test (a real test tag whose signed release `mesh node update` then verifies) belongs to a later item, since the updater doesn't exist yet.
