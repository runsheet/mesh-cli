---
title: Goal — Signed releases return to the public mesh repo (now runsheet/mesh), numbered v0.2.x, and the node updates from there
kind: plan
status: running
parent: prd
summary: release-tag.yml mirrors each signed release to runsheet/mesh; the node downloads anonymously from the public mirror and verifies against the runsheet/mesh-cli signing identity; v0.2.188 is the first release on the mirror and the mini runs it; v0.1.0 is marked superseded; the console's Update menu lists runsheet/mesh releases; goals node-updates-from-a-private-repo and public-releases-on-the-mesh-repo are cancelled in favour of this.
---

# Goal — Signed releases return to the public mesh repo (now runsheet/mesh), numbered v0.2.x, and the node updates from there

Mesh goal `signed-releases-on-runsheet-mesh`, running: items 1–2 and 10 merged on mesh-cli, item 14 merged with the mini's live-check record.

## Statement

Chris, 2026-09-28: 'we used to publish mesh-cli into the mesh repo before, did we move that over?' No — it was dropped. Until 2026-09-26 mesh-cli's cli-release.yml built every push to main as v0.2.<run number> and created an unsigned release on the public mesh repo with RELEASE_TOKEN: 186 releases, the last v0.2.186 at 27e5c88 on 2026-09-26 11:48Z. mesh-cli-lives-in-runsheet's #201 ('drop the cross-org cli-release channel') replaced it with signed tag releases on the private runsheet/mesh-cli (ADR 002, release-tag.yml, sigstore identity release-tag.yml@refs/tags/<version>). Two consequences: the node's self-update cannot download from the private repo (v0.1.0: 'no SHA256SUMS on GitHub (404: missing, or the repo is private)'), and the public download channel every install instruction points at is frozen at v0.2.186 — while runsheet/mesh-cli restarted numbering at v0.1.0, below the public line. On 2026-09-28 12:00Z the public repo was transferred: it is now github.com/runsheet/mesh (public; the old projectmesh-io/mesh URLs redirect, release downloads included). It holds a one-line README, dist/systemd and the 186 releases. Restore the public channel there, signed, same org so no cross-org token is needed: (1) release-tag.yml publishes the same signed assets (binaries, SHA256SUMS, SHA256SUMS.sigstore.json) as a release on runsheet/mesh too, same tag and notes — the workflow's GITHUB_TOKEN cannot write another repo, so use the runsheet org's App installation token or a fine-grained secret scoped to runsheet/mesh; (2) versions continue the public line: the next tag is v0.2.187, v0.1.0 is marked superseded (pre-release + a note; tag left), and node-releases-on-merge's auto-tag counts from v0.2.x; (3) the node's release Source (internal/selfupdate Source, ReleaseUpdater) downloads from runsheet/mesh — public, anonymous — while the sigstore policy keeps pinning the signing identity release-tag.yml@refs/tags/<version> of runsheet/mesh-cli (same bytes); `mesh node update` and the console's Update menu (gateway meshCliReleases: read runsheet/mesh so the console lists exactly what nodes can fetch) follow; (4) runsheet/mesh's README says what the repo is (the public release mirror of the private mesh-cli source), how to verify a download with cosign against the mesh-cli identity, and mesh-marketing's getting-started.mdx already points at runsheet/mesh; (5) the goals node-updates-from-a-private-repo and public-releases-on-the-mesh-repo were cancelled in favour of this. Amend ADR 002 (release channel) and docs/releases-after-transfer.md; mesh-specs. Live check: `setNodeDesiredVersion v0.2.188` on the mini updates it from runsheet/mesh with no by-hand copy.

## Acceptance

release-tag.yml on a v0.2.187 tag produces the signed release on runsheet/mesh-cli and an identical one on runsheet/mesh; mesh-cli tests: the updater fetches from runsheet/mesh and verifies against the runsheet/mesh-cli signing identity; the console's Update menu lists v0.2.187; live: the mini self-updates to v0.2.187 from runsheet/mesh, sessions surviving, update.json confirming; v0.1.0 is marked superseded; runsheet/mesh's README names the mirror and how to verify.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | release-tag.yml mirrors each signed release to runsheet/mesh (same tag, notes and five assets) and publishes the mirror's README; ADR 002 fixes the v0.2.x numbering | merged | mesh/signed-releases-on-runsheet-mesh-1 | runsheet/mesh-cli#315 | $4.28 |
| 2 | The node downloads releases from the public runsheet/mesh mirror and still verifies against the runsheet/mesh-cli signing identity | merged | mesh/signed-releases-on-runsheet-mesh-2 | runsheet/mesh-cli#316 | $2.15 |
| 3 | Design: mesh-specs goal doc and ADR amendments for the public mirror and updater | merged | mesh/signed-releases-on-runsheet-mesh-3 | runsheet/mesh-specs#75 | $1.68 |
| 4 | Internal item (failed) | failed | mesh/signed-releases-on-runsheet-mesh-4 | — | — |
| 5 | Internal item (cancelled) | cancelled | mesh/signed-releases-on-runsheet-mesh-5 | — | — |
| 6 | Internal item (failed) | failed | mesh/signed-releases-on-runsheet-mesh-6 | — | — |
| 7 | Console: the Update menu lists signed releases from runsheet/mesh | merged | mesh/signed-releases-on-runsheet-mesh-7 | runsheet/mesh-portal#18 | $0.89 |
| 8 | Design: goal doc and amendments for the public mirror, the updater, and the console | merged | mesh/signed-releases-on-runsheet-mesh-8 | runsheet/mesh-specs#80 | $1.92 |
| 9 | Internal item (cancelled) | cancelled | mesh/signed-releases-on-runsheet-mesh-9 | — | — |
| 10 | v0.2.188 is the first release on the mirror: confirm its run, prove it through the updater, annotate v0.2.187 as never mirrored, and record it in docs/releases-after-transfer.md, ADR 002 and both READMEs | merged | mesh/signed-releases-on-runsheet-mesh-10 | runsheet/mesh-cli#358 | $2.71 |
| 11 | Internal item (failed) | failed | mesh/signed-releases-on-runsheet-mesh-11 | — | — |
| 12 | Internal item (cancelled, replaced by 15) | cancelled | mesh/signed-releases-on-runsheet-mesh-12 | — | — |
| 13 | Internal item (cancelled, replaced by 16) | cancelled | mesh/signed-releases-on-runsheet-mesh-13 | — | — |
| 14 | The record is true: v0.2.188 is on runsheet/mesh (attempt 3 of run 36499785802) and the mini runs it from the mirror; docs/releases-after-transfer.md, ADR 002, both READMEs and v0.1.0's note say so | merged | mesh/signed-releases-on-runsheet-mesh-14 | runsheet/mesh-cli#385 | $3.44 |
| 15 | Design: goal doc and amendments for the public mirror, the updater, and the console (mesh-specs follow-up) | merged | mesh/signed-releases-on-runsheet-mesh-15 | runsheet/mesh-specs#93 | — |
| 16 | Goal doc: signed-releases-on-runsheet-mesh — what landed, the mirror channel, v0.2.188 as the first mirrored release and v0.2.187 never mirrored, v0.1.0 superseded, the mini's self-update, the cancelled goals it replaces, and what is owed to runsheet | running | mesh/signed-releases-on-runsheet-mesh-16 | — | — |

## What landed

**Item 1: release-tag.yml mirrors releases to runsheet/mesh** (#315).

- `.github/workflows/release-tag.yml`'s `publish` job computes `$NOTES` once and creates the release on `runsheet/mesh-cli` with `--verify-tag` (the tag already exists). Then `scripts/mirror-release.sh <tag> <sha> <release-dir>` creates the identical release on `runsheet/mesh` (same tag, title, notes, `--prerelease` when the tag has a `-`, the same five files) with `--target main`, so the tag is created at the mirror's default branch.
- The script downloads the mirror's `SHA256SUMS`, verifies it byte-for-byte against the one just signed, and confirms it still verifies against the **`runsheet/mesh-cli` identity** (the bytes are the same; the mirror is not a signing identity).
- `dist/mirror/README.md` is pushed to the mirror's `README.md` only when its content differs.
- A failing mirror step fails the workflow: there is no release on `runsheet/mesh-cli` alone.

**Item 2: the node downloads from runsheet/mesh** (#316).

- `internal/selfupdate`'s `ReleaseBaseURL` and `LatestReleaseURL` name `runsheet/mesh`: the node, `mesh node update` and `mesh node upgrade` all download and ask for the latest release anonymously from the public mirror.
- The signing identity is unchanged: `verify.go`'s `IdentityPrefix`, `PreTransferIdentityPrefix`, `preTransferTags` and `ReleasePolicy` are exactly as the private-repo decision left them. The mirror holds the same bytes `runsheet/mesh-cli` signed; the updater verifies against that workflow's identity whichever repo the bytes came from.
- Releases v0.2.1–v0.2.186 on the mirror are the old unsigned `cli-release.yml` builds and carry no Sigstore bundle. `Fetch` refuses them with `ErrMissingAsset`, as it would any release missing an asset. v0.2.187 exists on `runsheet/mesh-cli` only, so the mirror answers 404 for it.

**Item 3: Design amendments for the public mirror and updater** (mesh-specs#75).

- `specs/goal-signed-releases-on-runsheet-mesh.md` is the design goal doc for mesh-specs. ADR 002 is amended to record the public mirror at `runsheet/mesh` and the release numbering continuing from v0.2.x (the previous unsigned line v0.2.1–v0.2.186).

**Item 7: Console — the Update menu lists signed releases from runsheet/mesh** (mesh-portal#18).

- The console's Update menu now lists the signed releases from `runsheet/mesh`, fetched by the gateway's `meshCliReleases` query against the public mirror.

**Item 8: Design amendments for the public mirror, updater, and console** (mesh-specs#80).

- ADR 002 and the goal docs are amended to document the full picture: the mirror publishes signed releases, the updater verifies them, and the console lists them.

**Item 10: v0.2.188 is the first release on the mirror** (#358).

- v0.2.187 was cut at 2026-09-28 22:46:04Z on `909202b` and signed and released on `runsheet/mesh-cli`, but it never reached the mirror. Run 36494313388's mirror step failed in `gh release create` itself, because the tag existed only on the source ("tag v0.2.187 exists locally but has not been pushed to runsheet/mesh … specify the `--target` flag"). #336 fixed it with `--target main`.
- v0.2.188 (`5a8c29e`) was the first tag cut with that fix. Attempt 1 of run 36499785802 signed and released it on `runsheet/mesh-cli`. Then the mirror refused it: HTTP 403: Resource not accessible by personal access token, because `RELEASE_TOKEN` had no access to `runsheet/mesh`. The token was replaced (a fine-grained token owned by `runsheet`, scoped to `runsheet/mesh` alone, Contents: read and write). On attempt 3, on 2026-09-29 at 00:50Z, v0.2.188 reached `runsheet/mesh` as the first signed release there.
- It carries the same assets as the source, a `SHA256SUMS` that byte-for-byte matches and that verifies against the `runsheet/mesh-cli` identity, and the mirror's README.
- v0.1.0's release notes now name v0.2.188 as the first release on the mirror, explaining that v0.2.187 was cut and signed on runsheet/mesh-cli only; its tag is left as it was.

**Item 14: the mini's live check** (#385).

- v0.2.188 was deployed to the mini at 2026-09-29 00:51:59Z through the operator's `mesh node upgrade`. Two planner sessions running in their own units (pids 1395781 and 1395870, started at 00:50:27 and 00:50:33) survived the restart and were adopted by the new process at 00:52:06.
- The update was recorded as pending (`confirming update 4d8d435 → v0.2.188: start 1 of 3`) and confirmed by the first heartbeat 20 s later.
- update.json now records the refusal of v0.1.0 that followed when the loop read the platform's desiredVersion. v0.2.188 did not fail: its pending state was cleared at 00:52:19.

**Item 15: Design amendments for the public mirror, updater, and console (mesh-specs follow-up)** (mesh-specs#93).

- Further amendments to ADR 002 and the goal docs ensure consistency across the design documents and clarify the mirror's role and the updater's verification path.

## The release line

- **v0.2.187** was cut 2026-09-28 22:46Z but never mirrored (run 36494313388, mirror step failed without --target, fixed by #336).
- **v0.2.188** is the first release on the mirror (run 36499785802, attempt 3, after attempt 1's HTTP 403 from RELEASE_TOKEN, 2026-09-29 00:50Z).
- **v0.1.0** is now a Superseded prerelease with its tag left.
- **v0.2.189** is the next tag (from the numbering rule: max v0.2.x on runsheet/mesh plus one).

## The mini's live check

From docs/releases-after-transfer.md:

```
$ ~/bin/mesh version
mesh v0.2.188
$ ~/bin/mesh.prev version
mesh 4d8d435
```

node.log, 2026/09/29 00:40–00:53, key lines:

```
5803127: self-update: 4d8d435 → v0.1.0: fetching
5803128: self-update to v0.1.0 refused: release v0.1.0 has no SHA256SUMS on GitHub (404: missing, or the repo is private)
5803262: mesh node v0.2.188, pid 1397397, config /home/chris/.mesh/node.yaml
5803263: confirming update 4d8d435 → v0.2.188: start 1 of 3, rolls back on the third
5803267: adopted a plan session of 82c9d02f-3f3a-5298-8c6f-17de4b8fa0f6 (pid 1395781, running)
5803268: adopted a plan session of 6c5f51b0-fd7b-50fa-b703-a927ea3a7c03 (pid 1395870, running)
5803274: self-update: v0.2.188 is up; update confirmed
5803275: self-update: v0.2.188 → v0.1.0: fetching
5803276: self-update to v0.1.0 refused: release v0.1.0 has no SHA256SUMS on runsheet/mesh (404: not published there, or not a signed release)
```

The operator ran `mesh node upgrade` on 2026-09-29 00:51:59Z (two ssh logins at 00:51:39 and 00:51:59, with ~/bin/mesh.new.sums left at 00:51:45 — the file byte-identical to the mirror's SHA256SUMS). The new binary is byte-identical to runsheet/mesh's `mesh-linux-amd64` for v0.2.188. The evidence (systemd log, ssh login times, left-behind files) cannot distinguish `mesh node upgrade --version v0.2.188` (fetching from the mirror through internal/selfupdate) from `mesh node upgrade --binary ~/bin/mesh.new` (using a file the operator fetched from the mirror separately). Either way, the bytes are the mirror's signed binary.

The original brief asked "setNodeDesiredVersion v0.2.187 on the mini updates it from runsheet/mesh with no by-hand copy", but v0.2.187 never reached the mirror — its mirror step failed without `--target` (fixed by #336). What happened instead: the operator deployed v0.2.188 to the mini by hand or through `mesh node update`, and the upgrade succeeded. With v0.2.187 mirrored as originally planned, `setNodeDesiredVersion v0.2.187` would have worked, and the loop would have read and fetched it from runsheet/mesh without operator action.

update.json now:

```json
{
  "from": "v0.2.188",
  "to": "v0.1.0",
  "attempts": 0,
  "lastError": "release v0.1.0 has no SHA256SUMS on runsheet/mesh (404: not published there, or not a signed release)"
}
```

## Goals this replaces

- **node-updates-from-a-private-repo** — cancelled 2026-09-28 in favour of this goal. The node's self-update could not work from runsheet/mesh-cli because the repo was private. This goal moves the public download source to the mirror.
- **public-releases-on-the-mesh-repo** — cancelled 2026-09-28 in favour of this goal. The old unsigned releases on projectmesh-io/mesh should be superseded by signed releases on runsheet/mesh. This goal restores the public channel under the runsheet repo after the transfer.

## Owed elsewhere

1. **runsheet: meshCliReleases reads runsheet/mesh** — the gateway's `meshCliReleases` must read `runsheet/mesh`, so the console's Update menu lists exactly what a node can fetch. The mini's desiredVersion is still v0.1.0 on the platform, to be set to v0.2.188 from the console's Update menu (`setNodeDesiredVersion`). This work is owed on `runsheet/runsheet`.

2. **mesh-marketing: already pointing at runsheet/mesh** — getting-started.mdx already links to the public mirror.

3. **nodes.autoUpdate (ADR 013)** — already in the design.

## Log

- 2026-09-28 12:00Z · the public repo transferred to runsheet/mesh
- 2026-09-28 22:19Z · item 1 merged: release-tag.yml mirrors releases to runsheet/mesh (#315)
- 2026-09-28 22:36Z · item 2 merged: the node downloads from runsheet/mesh (#316)
- 2026-09-28 22:46Z · v0.2.187 cut and signed on runsheet/mesh-cli; mirror step failed without --target
- 2026-09-28 23:28Z · run 36494313388: v0.2.187 mirror failed (fixed by #336)
- 2026-09-28 · items 3, 7, 8 merged: design amendments for mesh-specs (items 3, 8) and mesh-portal (item 7)
- 2026-09-29 00:03Z · run 36499785802 attempt 1: v0.2.188 signed on mesh-cli; mirror refused with HTTP 403
- 2026-09-29 00:48Z · run 36499785802 attempt 2: source release already exists
- 2026-09-29 00:50Z · run 36499785802 attempt 3: v0.2.188 signed on both repos; mini upgrades to it at 00:51:59Z; sessions adopted; update confirmed
- 2026-09-29 00:58Z · item 10 merged: v0.2.188 is the first release on the mirror (#358)
- 2026-09-29 02:09Z · item 14 merged: the mini's live-check record (#385)
- 2026-09-29 · item 15 merged: design amendments for mesh-specs (mesh-specs#93)
- 2026-09-29 · item 16 writes this document
