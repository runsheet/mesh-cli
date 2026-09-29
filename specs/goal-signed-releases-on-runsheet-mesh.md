---
title: Goal — Signed releases return to the public mesh repo (now runsheet/mesh), numbered v0.2.x, and the node updates from there
kind: plan
status: draft
parent: prd
summary: Releases mirror to runsheet/mesh from v0.2.188, the mini runs it, and meshCliReleases on runsheet is owed.
---

# Goal — Signed releases return to the public mesh repo (now runsheet/mesh), numbered v0.2.x, and the node updates from there

Mesh goal `signed-releases-on-runsheet-mesh`, running.

## Statement

Chris, 2026-09-28: 'we used to publish mesh-cli into the mesh repo before, did we move that over?' No — it was dropped. Until 2026-09-26 mesh-cli's cli-release.yml built every push to main as v0.2.<run number> and created an unsigned release on the public mesh repo with RELEASE_TOKEN: 186 releases, the last v0.2.186 at 27e5c88 on 2026-09-26 11:48Z. mesh-cli-lives-in-runsheet's #201 ('drop the cross-org cli-release channel') replaced it with signed tag releases on the private runsheet/mesh-cli (ADR 002, release-tag.yml, sigstore identity release-tag.yml@refs/tags/<version>). Two consequences: the node's self-update cannot download from the private repo (v0.1.0: 'no SHA256SUMS on GitHub (404: missing, or the repo is private)'), and the public download channel every install instruction points at is frozen at v0.2.186 — while runsheet/mesh-cli restarted numbering at v0.1.0, below the public line. On 2026-09-28 12:00Z the public repo was transferred: it is now github.com/runsheet/mesh (public; the old projectmesh-io/mesh URLs redirect, release downloads included). It holds a one-line README, dist/systemd and the 186 releases. Restore the public channel there, signed, same org so no cross-org token is needed: (1) release-tag.yml publishes the same signed assets (binaries, SHA256SUMS, SHA256SUMS.sigstore.json) as a release on runsheet/mesh too, same tag and notes — the workflow's GITHUB_TOKEN cannot write another repo, so use the runsheet org's App installation token or a fine-grained secret scoped to runsheet/mesh; (2) versions continue the public line: the next tag is v0.2.187, v0.1.0 is marked superseded (pre-release + a note; tag left), and node-releases-on-merge's auto-tag counts from v0.2.x; (3) the node's release Source (internal/selfupdate Source, ReleaseUpdater) downloads from runsheet/mesh — public, anonymous — while the sigstore policy keeps pinning the signing identity release-tag.yml@refs/tags/<version> of runsheet/mesh-cli (same bytes); `mesh node update` and the console's Update menu (gateway meshCliReleases: read runsheet/mesh so the console lists exactly what nodes can fetch) follow; (4) runsheet/mesh's README says what the repo is (the public release mirror of the private mesh-cli source), how to verify a download with cosign against the mesh-cli identity, and mesh-marketing's getting-started.mdx already points at runsheet/mesh; (5) the goals node-updates-from-a-private-repo and public-releases-on-the-mesh-repo were cancelled in favour of this. Amend ADR 002 (release channel) and docs/releases-after-transfer.md; mesh-specs. Live check: `setNodeDesiredVersion v0.2.187` on the mini updates it from runsheet/mesh with no by-hand copy.

## Acceptance

release-tag.yml on a v0.2.187 tag produces the signed release on runsheet/mesh-cli and an identical one on runsheet/mesh; mesh-cli tests: the updater fetches from runsheet/mesh and verifies against the runsheet/mesh-cli signing identity; the console's Update menu lists v0.2.187; live: the mini self-updates to v0.2.187 from runsheet/mesh, sessions surviving, update.json confirming; v0.1.0 is marked superseded; runsheet/mesh's README names the mirror and how to verify.

**How it was met.** v0.2.188 stood in for v0.2.187 throughout: v0.2.187 was cut and signed on runsheet/mesh-cli but never mirrored, because its mirror step failed without `--target` (#336 fixed the cause), so v0.2.188 is the first release on runsheet/mesh and the one the acceptance's live check holds against. The console's Update menu clause — gateway `meshCliReleases` reading runsheet/mesh — is owed to runsheet, not met here (see Owed).

## Items

- **Item 1** (merged): release-tag.yml mirrors each signed release to runsheet/mesh (same tag, notes and five assets) and publishes the mirror's README; ADR 002 fixes the v0.2.x numbering — [mesh-cli#315](https://github.com/runsheet/mesh-cli/pull/315).
- **Item 2** (merged): The node downloads releases from the public runsheet/mesh mirror and still verifies against the runsheet/mesh-cli signing identity — [mesh-cli#316](https://github.com/runsheet/mesh-cli/pull/316).
- **Item 3** (merged): mesh-specs: the TRD's node section and ADR 007/008 name runsheet/mesh as the public release mirror nodes download from, signed by runsheet/mesh-cli — [mesh-specs#75](https://github.com/runsheet/mesh-specs/pull/75).
- **Item 4** (failed): Cut v0.2.187 from main, mirror it to runsheet/mesh, mark v0.1.0 superseded, and update the mini from the mirror; record it in docs/releases-after-transfer.md — item 10 did its work instead.
- **Item 5** (cancelled): Goal doc: signed-releases-on-runsheet-mesh — what landed, the mirror channel, the cancelled goals it replaces, and the runsheet half owed (meshCliReleases reading runsheet/mesh).
- **Item 6** (failed): Cut v0.2.187 from main, prove the mirror release on runsheet/mesh through the node's own updater, mark v0.1.0 superseded, and record it in docs/releases-after-transfer.md — item 10 did its work instead.
- **Item 7** (merged): mesh-portal: ADR 005 and the node record's comments say the releases the console counts against are runsheet/mesh's, the public mirror nodes fetch from — [mesh-portal#18](https://github.com/runsheet/mesh-portal/pull/18).
- **Item 8** (merged): mesh-specs: ADR 013's auto-tag counts from the public line on runsheet/mesh (v0.2.x), and the TRD's release paragraph links the record of v0.2.187 — [mesh-specs#80](https://github.com/runsheet/mesh-specs/pull/80).
- **Item 9** (cancelled): Goal doc: signed-releases-on-runsheet-mesh — what landed, the mirror channel, v0.2.187 and the superseded v0.1.0, the cancelled goals it replaces, the mini's live check, and the runsheet half owed.
- **Item 10** (merged): v0.2.188 is the first release on the mirror: confirm its run, prove it through the updater, annotate v0.2.187 as never mirrored, and record it in docs/releases-after-transfer.md, ADR 002 and both READMEs — [mesh-cli#358](https://github.com/runsheet/mesh-cli/pull/358) (e2978a0).
- **Item 11** (failed): The mini upgrades to v0.2.188 from runsheet/mesh through its own updater, sessions surviving, and docs/releases-after-transfer.md records update.json and the adopt lines — items 14 and 21 did its work instead between them.
- **Item 12** (failed): mesh-specs: ADR 013's amendment and the TRD name v0.2.188 as the first release on runsheet/mesh, v0.2.187 as cut but never mirrored, and v0.2.189 as the first auto-cut tag — item 15 did its work instead.
- **Item 13** (cancelled): Goal doc: signed-releases-on-runsheet-mesh — what landed, the mirror channel, v0.2.188 as the first mirrored release and v0.2.187 never mirrored, v0.1.0 superseded, the mini's bootstrap, the cancelled goals it replaces, and the runsheet half owed.
- **Item 14** (merged): The record is true: v0.2.188 is on runsheet/mesh (attempt 3 of run 36499785802) and the mini runs it from the mirror; docs/releases-after-transfer.md, ADR 002, both READMEs and v0.1.0's note say so — [mesh-cli#385](https://github.com/runsheet/mesh-cli/pull/385) (c3b6cb8).
- **Item 15** (merged): mesh-specs: ADR 013's amendment and the TRD name v0.2.188 as the first release on runsheet/mesh, v0.2.187 as cut but never mirrored, and v0.2.189 as the first auto-cut tag — [mesh-specs#93](https://github.com/runsheet/mesh-specs/pull/93) (365cb49).
- **Item 16** (cancelled): Goal doc: signed-releases-on-runsheet-mesh — what landed, the mirror channel, v0.2.188 as the first mirrored release and v0.2.187 never mirrored, v0.1.0 superseded, the mini's self-update, the cancelled goals it replaces, and what is owed to runsheet.
- **Item 17** (cancelled): The record of v0.2.188 on the mirror and the mini's self-update is true and merges: docs/releases-after-transfer.md, ADR 002, both READMEs and .runsheet/validate.yml (carry PR #385 forward).
- **Item 18** (failed): mesh-specs: ADR 013's amendment and the TRD name v0.2.188 as the first release on the mirror, v0.2.187 as cut but never mirrored, and v0.2.189 as the first auto-cut tag (carry PR #93 forward) — item 15 did its work instead.
- **Item 19** (cancelled): Goal doc: signed-releases-on-runsheet-mesh — what landed, the mirror channel, v0.2.188 first on the mirror and v0.2.187 never mirrored, v0.1.0 superseded, the mini's self-update, the cancelled goals it replaces, and what is owed to runsheet.
- **Item 20** (failed): Goal doc: signed-releases-on-runsheet-mesh — what landed, v0.2.188 first on the mirror and v0.2.187 never mirrored, v0.1.0 superseded, the mini's upgrade as the record shows it, the cancelled goals it replaces, and what is owed — item 22 did its work instead.
- **Item 21** (merged): docs/releases-after-transfer.md: a dated addendum of the mirror channel's state on 2026-09-29 — desiredVersion, update.json, the mirror README's lag, and the auto-tag blocked on RELEASE_TAG_TOKEN — [mesh-cli#390](https://github.com/runsheet/mesh-cli/pull/390) (1a57ab0).
- **Item 22** (this item): Goal doc: signed-releases-on-runsheet-mesh — what landed, the mirror channel, v0.2.188 first on the mirror and v0.2.187 never mirrored, v0.1.0 superseded, the mini on v0.2.188, the replaced goals and what is owed.

## The mirror channel

release-tag.yml's `publish` job has a "Mirror the release to runsheet/mesh" step running scripts/mirror-release.sh with RELEASE_TOKEN, creating the same tag, notes and five assets on runsheet/mesh at `--target main` (#336, 5a8c29e), and pushing dist/mirror/README.md as the mirror's README. The node's internal/selfupdate `ReleaseBaseURL` is `https://github.com/runsheet/mesh/releases/download/` and `LatestReleaseURL` reads runsheet/mesh, anonymous, while the sigstore policy pins `https://github.com/runsheet/mesh-cli/.github/workflows/release-tag.yml@refs/tags/<version>` — tests in internal/selfupdate/fetch_test.go and verify_test.go. The design amendments: mesh-cli ADR 002, mesh-specs TRD, ADR 007/008/013, mesh-portal ADR 005.

## Releases

**v0.2.187**: cut and signed on runsheet/mesh-cli only, run 36494313388, its mirror step failed without `--target`, never mirrored, off the public line. **v0.2.188**: the first signed release on runsheet/mesh, run 36499785802 attempt 3 on 2026-09-29 after attempt 1's HTTP 403 from RELEASE_TOKEN, present on both repos. **v0.1.0**: pre-release on runsheet/mesh-cli with a 'Superseded' note pointing at v0.2.188, tag left. **v0.2.189**: not cut — release-on-merge.yml run 36507379094 failed at its guard, RELEASE_TAG_TOKEN is not set, an operator step under goal node-releases-on-merge.

## The mini

The mini runs v0.2.188 from runsheet/mesh since 2026-09-29 00:52Z, desiredVersion v0.2.188, sessions adopted across the restart, update.json holding a stale v0.1.0 refusal that is expected between attempts. See docs/releases-after-transfer.md's "The mini updates from runsheet/mesh" and "desiredVersion, confirmed" sections for the evidence; the record cannot tell `--binary` from `--version` for the operator's upgrade, so this doc does not either.

## Replaced goals

**node-updates-from-a-private-repo**: cancelled in favour of this goal, which restored the node's update path by mirroring the signed release publicly instead of granting the node a private-repo credential. **public-releases-on-the-mesh-repo**: cancelled in favour of this goal, which reused the transferred runsheet/mesh repo as the mirror instead of standing up a new public channel.

## Owed

1. `meshCliReleases` on runsheet/runsheet reading runsheet/mesh, so the console's Update menu lists what nodes can fetch.
2. RELEASE_TAG_TOKEN on runsheet/mesh-cli, the operator's, goal node-releases-on-merge.
3. The live mirror README still reads 'From v0.2.187' until the next mirrored release pushes main's 'From v0.2.188'.
4. v0.2.187's stale release notes, the operator's call.

## Log

- 2026-09-28 · item 1 · mesh-cli#315 merged: release-tag.yml mirrors each release to runsheet/mesh; ADR 002 fixes the numbering.
- 2026-09-28 · item 2 · mesh-cli#316 merged: the node downloads from the mirror, verifies against the mesh-cli identity.
- 2026-09-28 · item 3 · mesh-specs#75 merged: the TRD and ADR 007/008 name runsheet/mesh as the mirror.
- 2026-09-28 · item 4 · failed: cutting and mirroring v0.2.187; item 10 did this instead.
- 2026-09-28 · item 6 · failed: proving the mirror through the updater; item 10 did this instead.
- 2026-09-28 · item 7 · mesh-portal#18 merged: ADR 005 and the node record name runsheet/mesh as the release count.
- 2026-09-28 · item 8 · mesh-specs#80 merged: ADR 013's auto-tag counts from runsheet/mesh's v0.2.x line.
- 2026-09-29 · item 10 · mesh-cli#358 merged (e2978a0): v0.2.188's run refused by the mirror's token (HTTP 403); v0.2.187 never mirrored.
- 2026-09-29 · item 11 · failed: the mini's upgrade record; items 14 and 21 did this instead.
- 2026-09-29 · item 12 · failed: naming v0.2.188 as the first release in mesh-specs; item 15 did this instead.
- 2026-09-29 · item 14 · mesh-cli#385 merged (c3b6cb8): the record is true, v0.2.188 is on runsheet/mesh and the mini runs it.
- 2026-09-29 · item 15 · mesh-specs#93 merged (365cb49): ADR 013 and the TRD name v0.2.188 as the first release on the mirror.
- 2026-09-29 · item 18 · failed: carrying mesh-specs#93 forward; item 15 did this instead.
- 2026-09-29 · item 20 · failed: an earlier attempt at this goal doc; item 22 did this instead.
- 2026-09-29 · item 21 · mesh-cli#390 merged (1a57ab0): the dated addendum of the mirror channel's state.
- 2026-09-29 · item 22 · this item: the goal doc.
