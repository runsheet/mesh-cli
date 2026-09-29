---
title: Every node-changing merge to mesh-cli main is a release the console offers, and a node can follow the latest by itself
kind: plan
status: draft
parent: prd
summary: release-on-merge, grouped notes, the tag-or-sha version and the console's releases-behind landed; the first auto-tag waits on RELEASE_TAG_TOKEN and nodes.autoUpdate is owed to runsheet.
---

# Every node-changing merge to mesh-cli main is a release the console offers, and a node can follow the latest by itself

Mesh goal `node-releases-on-merge`, running.

## Statement

Chris, 2026-09-28: the mini was found running mesh-cli 72ff910 while main was ten commits ahead (pitches, Codex evaluations, MCP call counts, tool-result trimming) — 'are we on an old version of mesh-cli?'. mesh-cli has no deploy: merged node goals do nothing until a binary reaches the mini, which has been a person cross-building and running `mesh node upgrade --binary` by hand (HANDOFF 2026-09-26 08:35Z), and a by-hand upgrade rolled back this morning under load. The proper path exists and was unused: update-nodes-from-console + node-self-update let the console offer GitHub releases of runsheet/mesh-cli and the node update itself to the version the platform asks for (Node.desired_version) — but the only releases were two v0.0.0-*-test pre-releases; nobody cut a real one until v0.1.0 today. Close the gap for good: (1) a merge to mesh-cli main that changes the node (anything under cmd/ or internal/ — not docs or specs alone) cuts a release automatically — a `release-on-merge` workflow bumps the patch version (minor when the merge's PR carries a `release:minor` label or the goal doc says so), tags, and lets release-tag.yml build the signed binaries; a docs-only merge cuts none; (2) the release notes are the merged PR titles since the last tag, grouped by goal slug, with the goal-doc link; (3) the platform's `mesh_health_nodes` and the console's Nodes page flag a node more than one release behind (node-behind-main-is-seen covers commits; make it releases too) and the Update button defaults to the latest; (4) an org-level setting `nodes.autoUpdate` (default off) lets the platform set desired_version to the latest release itself when the node is idle, so the mini follows main within an hour with no person — with the confirm-window fix from upgrades-survive-a-slow-platform so a busy platform does not roll it back; (5) the node's reported version is the release tag (v0.1.1) when built from one, the sha otherwise, and the console shows both. Amend mesh-specs (release path, ADR on node-self-update) and runsheet's HANDOFF 'Mesh node deploys' section so it stops saying 'by hand'.

## Acceptance

A merge touching cmd/ or internal/ on mesh-cli main produces a tagged release with signed binaries within 15 minutes and release notes grouped by goal; a docs-only merge produces none (workflow tests or a dry run in the PR); the console's Nodes page offers the new release and flags the mini as behind until it updates; with nodes.autoUpdate on, the mini updates itself from a release with no person and its sessions survive (e2e with the fake GitHub for the platform half; live check on the mini). HANDOFF no longer describes the by-hand path except as the fallback.

**How it stands.** The mesh-cli half has landed: release-on-merge.yml and its script (#330, re-landed as #337), grouped release notes (#317), the tag-or-sha version with `commit` on the heartbeat (#402), and the README and docs/rollout.md naming the release path with the by-hand upgrade as the fallback (#401); the console shows the tag and commit and flags a node releases behind (mesh-portal#16, #18), and mesh-specs records the path (mesh-specs#76). Not met yet: no merge has produced a release, because RELEASE_TAG_TOKEN is unset (see Releases); `nodes.autoUpdate`, the releases-behind count on the platform, the Update button's default, the e2e with the fake GitHub and HANDOFF's section are owed to runsheet/runsheet (see Owed), so the live check on the mini cannot run.

## Items

Statuses as Mesh recorded them at 2026-09-29T08:46:50Z (mesh_items, project runsheet/mesh, goal node-releases-on-merge); every status below matches the brief.

- **Item 1** (merged): release-on-merge: a node-changing merge to main bumps the version, tags, and release-tag.yml builds it; a docs-only merge cuts none — [mesh-cli#330](https://github.com/runsheet/mesh-cli/pull/330). Evaluation passed.
- **Item 2** (merged): release-tag.yml release notes: the merged PR titles since the last release, grouped by goal slug with the goal-doc link — [mesh-cli#317](https://github.com/runsheet/mesh-cli/pull/317).
- **Item 3** (failed): Node: the reported version is the release tag when built from one, the commit sha otherwise, and the heartbeat carries `commit` — no pull request merged; its session ran out of budget, and the work landed through items 8 and 12.
- **Item 4** (merged): Console: the Nodes list and node page show the release tag and commit, and flag a node more than one release behind with the latest named — [mesh-portal#16](https://github.com/runsheet/mesh-portal/pull/16), amended by [mesh-portal#18](https://github.com/runsheet/mesh-portal/pull/18).
- **Item 5** (merged): mesh-specs: TRD names the release path, `commit`, `behind`, and nodes.autoUpdate; ADR 013 — a node-changing merge is a release and a node follows the latest by itself — [mesh-specs#76](https://github.com/runsheet/mesh-specs/pull/76).
- **Item 6** (cancelled): Goal doc: node-releases-on-merge — what landed, the first automatic release, and the platform half owed to runsheet (autoUpdate, releases-behind, HANDOFF) — no pull request.
- **Item 7** (merged): release-on-merge: a node-changing merge to main bumps the version, tags, and release-tag.yml builds it; a docs-only merge cuts none — [mesh-cli#337](https://github.com/runsheet/mesh-cli/pull/337). It re-landed the same workflow on top of #330.
- **Item 8** (cancelled): Node: the reported version is the release tag when built from one, the commit sha otherwise, and the heartbeat carries `commit` when the gateway takes it — [mesh-cli#338](https://github.com/runsheet/mesh-cli/pull/338), closed unmerged and carried forward as item 12.
- **Item 9** (failed): Goal doc: node-releases-on-merge — what landed, the first automatic release, and the platform half owed to runsheet (autoUpdate, releases-behind, Update-to-latest, HeartbeatNodeInput.commit, HANDOFF) — no pull request merged; evaluation failed 3 times.
- **Item 10** (failed): mesh-cli README and docs/rollout.md: a merge is a release and the mini follows it; the by-hand upgrade is the fallback, not the way — no pull request merged; its GitHub token was rate-limited, and it was redone as item 13.
- **Item 11** (cancelled): Goal doc: node-releases-on-merge — what landed, the first automatic release blocked on RELEASE_TAG_TOKEN, item 8's PR as it stands, and the platform half owed to runsheet — no pull request; replaced by this plan.
- **Item 12** (merged): Node: the reported version is the release tag when built from one, the commit sha otherwise, and the heartbeat carries `commit` when the gateway takes it (carry PR #338 forward onto main) — [mesh-cli#402](https://github.com/runsheet/mesh-cli/pull/402).
- **Item 13** (merged): mesh-cli README and docs/rollout.md: a merge is a release and the mini follows it; the by-hand `--binary` upgrade is the fallback, not the way — [mesh-cli#401](https://github.com/runsheet/mesh-cli/pull/401).
- **Item 14** (failed): Goal doc: node-releases-on-merge — every item's PR and status as Mesh records them, the first automatic release blocked on RELEASE_TAG_TOKEN, the mini's path, and what is owed to runsheet and to Chris — no pull request merged; evaluation failed 3 times.
- **Item 15** (running): Goal doc: node-releases-on-merge — what landed and where, the first automatic release blocked on RELEASE_TAG_TOKEN, the mini's update paths, and the platform half owed to runsheet and to Chris — this item, this document.

## Releases

runsheet/mesh-cli's releases are **v0.2.188** (latest, 2026-09-29 00:50Z, the first on the public mirror runsheet/mesh), **v0.2.187** (cut, never mirrored) and **v0.1.0** (pre-release, superseded). The public line continues from v0.2.x per mesh-specs ADR 013 and mesh-cli ADR 002.

No automatic release has been cut yet. The Release on merge workflow (`.github/workflows/release-on-merge.yml`) ran on main three times — runs 36507379094, 36533387461 and 36538912746 — and each failed at the step 'Require RELEASE_TAG_TOKEN', because the repository secret RELEASE_TAG_TOKEN is unset (`gh run list --repo runsheet/mesh-cli --workflow release-on-merge.yml` shows no passing run as of 2026-09-29T08:46Z).

What Chris must do: create a fine-grained token with Contents: read and write on runsheet/mesh-cli and add it as the repository secret RELEASE_TAG_TOKEN (`GITHUB_TOKEN` will not do: a tag it pushes starts no workflow). The next node-changing merge — or `gh workflow run release-on-merge.yml -f dry_run=false` — then tags v0.2.189, and release-tag.yml builds, signs and mirrors it.

A docs-only merge cuts none: only a merge touching `cmd/`, `internal/`, `images/node/`, `go.mod` or `go.sum` is tagged. The tests are `scripts/release-on-merge_test.sh` (the bump and the paths that count) and `scripts/release-notes_test.sh` (the notes grouped by goal slug).

## The mini

The normal path is a release. The console sets the node's `desiredVersion` and the node self-updates: it verifies the release, swaps its binary, restarts at once and adopts its running sessions — no drain. Or, by hand from the release, `mesh node update <tag>`, which drains first — it waits for the node's sessions to end — then swaps, so nothing is adopted. The fallback, only when neither works (the workflow cannot run, or a change must reach the mini before the next tag), is cross-building for linux/amd64 and running `mesh node upgrade --binary`, which swaps and restarts without a drain and adopts the host sessions, as a self-update does.

The mini runs v0.2.188 today (`mesh_health_nodes` at 2026-09-29T08:46:44Z: version v0.2.188, desired_version v0.2.188, behind, 15 commits behind main). It cannot follow main until RELEASE_TAG_TOKEN is set, so merges become releases, and `nodes.autoUpdate` lands on the platform, so no person has to set desiredVersion.

## Owed

To runsheet/runsheet:

1. The org setting `nodes.autoUpdate` (default off): the platform sets desired_version to the latest release when the node is idle, skipping a node whose record carries `updateError`, with the confirm-window from upgrades-survive-a-slow-platform.
2. `behind` computed in releases — `behind.releases` and `behind.latest` — on the node record and in `mesh_health_nodes`.
3. The console's Update button defaulting to the latest release.
4. `HeartbeatNodeInput.commit` on the gateway; the node already sends `commit` when the schema takes it.
5. `meshCliReleases` reading runsheet/mesh.
6. The e2e with the fake GitHub.
7. HANDOFF's 'Mesh node deploys' section saying releases, with by-hand only as the fallback.

To Chris:

1. The RELEASE_TAG_TOKEN secret on runsheet/mesh-cli.
2. Turning `nodes.autoUpdate` on for the mini once it exists.
