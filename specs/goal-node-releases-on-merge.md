---
title: Goal — Every node-changing merge to mesh-cli main is a release the console offers, and a node can follow the latest by itself
kind: plan
status: draft
parent: prd
summary: A merge to mesh-cli main that changes the node (cmd/ or internal/) cuts a release automatically — release-on-merge bumps the patch version (minor with release:minor label or goal doc), tags, and release-tag.yml builds signed binaries; docs-only merges cut none; release notes group merged PR titles by goal slug with the goal-doc link; the console flags the node as behind and defaults the Update button to the latest; nodes.autoUpdate (default off) lets the platform update the mini itself to the latest release when idle within an hour.
---

# Goal — Every node-changing merge to mesh-cli main is a release the console offers, and a node can follow the latest by itself

Mesh goal `node-releases-on-merge`, in progress.

## Statement

Chris, 2026-09-28: the mini was found running mesh-cli 72ff910 while main was ten commits ahead (pitches, Codex evaluations, MCP call counts, tool-result trimming) — 'are we on an old version of mesh-cli?'. mesh-cli has no deploy: merged node goals do nothing until a binary reaches the mini, which has been a person cross-building and running `mesh node upgrade --binary` by hand (HANDOFF 2026-09-26 08:35Z), and a by-hand upgrade rolled back this morning under load. The proper path exists and was unused: update-nodes-from-console + node-self-update let the console offer GitHub releases of runsheet/mesh-cli and the node update itself to the version the platform asks for (Node.desired_version) — but the only releases were two v0.0.0-*-test pre-releases; nobody cut a real one until v0.1.0 today. Close the gap for good: (1) a merge to mesh-cli main that changes the node (anything under cmd/ or internal/ — not docs or specs alone) cuts a release automatically — a `release-on-merge` workflow bumps the patch version (minor when the merge's PR carries a `release:minor` label or the goal doc says so), tags, and lets release-tag.yml build the signed binaries; a docs-only merge cuts none; (2) the release notes are the merged PR titles since the last tag, grouped by goal slug, with the goal-doc link; (3) the platform's `mesh_health_nodes` and the console's Nodes page flag a node more than one release behind (node-behind-main-is-seen covers commits; make it releases too) and the Update button defaults to the latest; (4) an org-level setting `nodes.autoUpdate` (default off) lets the platform set desired_version to the latest release itself when the node is idle, so the mini follows main within an hour with no person — with the confirm-window fix from upgrades-survive-a-slow-platform so a busy platform does not roll it back; (5) the node's reported version is the release tag (v0.1.1) when built from one, the sha otherwise, and the console shows both. Amend mesh-specs (release path, ADR on node-self-update) and runsheet's HANDOFF 'Mesh node deploys' section so it stops saying 'by hand'.

## Acceptance

A merge touching cmd/ or internal/ on mesh-cli main produces a tagged release with signed binaries within 15 minutes and release notes grouped by goal; a docs-only merge produces none (workflow tests or a dry run in the PR); the console's Nodes page offers the new release and flags the mini as behind until it updates; with nodes.autoUpdate on, the mini updates itself from a release with no person and its sessions survive (e2e with the fake GitHub for the platform half; live check on the mini). HANDOFF no longer describes the by-hand path except as the fallback.

## What landed

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 2 | release-tag.yml release notes: the merged PR titles since the last release, grouped by goal slug with the goal-doc link | merged | mesh/node-releases-on-merge-2 | https://github.com/runsheet/mesh-cli/pull/317 | $1.40 |
| 3 | (failed, redone as item 8) | — | — | — | — |
| 4 | Console: tag, commit, and behind (releases) — the Nodes page shows a node's release version (tag or sha), the commit, and whether it is behind the latest release | owed to runsheet/mesh | — | mesh-portal (not yet) | — |
| 5 | Design: node-releases-on-merge (adr-013 and trd §1 node and §3) | owed | — | mesh-specs (not yet) | — |
| 7 | release-on-merge: a node-changing merge to main bumps the version, tags, and release-tag.yml builds it; a docs-only merge cuts none | merged | mesh/node-releases-on-merge-7 | https://github.com/runsheet/mesh-cli/pull/330 | $2.84 |
| 8 | Node: the reported version is the release tag when built from one, the commit sha otherwise, and the heartbeat carries `commit` when the gateway takes it | open | mesh/node-releases-on-merge-8 | https://github.com/runsheet/mesh-cli/pull/338 | — |

## The first automatic release

The release-on-merge workflow runs on every push to main and computes the next tag (patch, or minor with `release:minor` label or goal doc marker), but it cannot tag yet: **the operator must add the `RELEASE_TAG_TOKEN` repository secret before the first node-changing merge tags anything**. It is a fine-grained personal access token with Contents: read and write on runsheet/mesh-cli.

With the secret in place, the workflow will tag the next merge that touches cmd/, internal/, images/node/, go.mod or go.sum; a docs-only merge produces no tag.

A `workflow_dispatch` dry run on the `release-on-merge` workflow prints the tag it would create next, for testing before the secret lands.

**As of 2026-09-29 03:00Z:** v0.2.187 and v0.2.188 have been cut manually by Chris Kolenko, not by the workflow. The workflow is on main and ready; it awaits the `RELEASE_TAG_TOKEN` secret to tag the next release automatically.

## Owed to runsheet/runsheet

The platform half this project cannot build (per ADR 013's "What is owed" section):

- **`HeartbeatNodeInput.commit` and `Node.commit`:** the node's field is ready and withheld only by introspection (so when the gateway schema gains it, the node accepts it at once).
- **`behind: { releases, latest }` computed from `meshCliReleases`:** the node's releases, the latest tag, and how many releases the node is behind, so `mesh_health_nodes` can flag a node more than one release behind and the console can show it.
- **`mesh_health_nodes` flagging a node more than one release behind:** alongside the existing commit-based behind check (node-behind-main-is-seen).
- **The console's Update button defaults to the latest release** rather than requiring the operator to select it by hand.
- **Org setting `nodes.autoUpdate` (default off):** when on, the platform sets desired_version to the latest release once an hour when the node is idle (no claims in progress).
- **HANDOFF 'Mesh node deploys' section:** amend it to say the by-hand path is the fallback, and the standard is now the node updating itself from releases (once autoUpdate and the confirm-window fix from upgrades-survive-a-slow-platform land).

## The mini

Until autoUpdate exists, a person sets desiredVersion from the console (or `mesh node update <version>`) to the latest release tag (e.g., v0.2.188). With the tag-or-sha version reporting done (item 8), the node's reported version will be the tag itself, not a commit sha. Building the binary is no longer a cross-build (the release is already signed and built by GitHub Actions), so `scp` and `mesh node upgrade --binary` by hand is the operation until the first autoUpdate tick.
