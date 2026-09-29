---
title: Goal — Signed releases return to the public mesh repo (now runsheet/mesh), numbered v0.2.x, and the node updates from there
kind: plan
status: running
parent: prd
summary: v0.2.188 (run 36499785802, attempt 3) is the first signed release on the public mirror runsheet/mesh; v0.2.187 was cut and signed on runsheet/mesh-cli only, its mirror step failing without --target; v0.1.0 is superseded; the mini runs v0.2.188 through the operator's mesh node upgrade, sessions adopted, update.json still holding the v0.1.0 refusal; the goals node-updates-from-a-private-repo and public-releases-on-the-mesh-repo are cancelled in favour of this one; meshCliReleases and RELEASE_TAG_TOKEN stay owed, the latter blocking v0.2.189 (run 36507379094).
---

# Goal — Signed releases return to the public mesh repo (now runsheet/mesh), numbered v0.2.x, and the node updates from there

Mesh goal `signed-releases-on-runsheet-mesh`

## Statement

Chris, 2026-09-28: 'we used to publish mesh-cli into the mesh repo before, did we move that over?' No — it was dropped. Until 2026-09-26 mesh-cli's cli-release.yml built every push to main as v0.2.<run number> and created an unsigned release on the public mesh repo with RELEASE_TOKEN: 186 releases, the last v0.2.186 at 27e5c88 on 2026-09-26 11:48Z. mesh-cli-lives-in-runsheet's #201 ('drop the cross-org cli-release channel') replaced it with signed tag releases on the private runsheet/mesh-cli (ADR 002, release-tag.yml, sigstore identity release-tag.yml@refs/tags/<version>). Two consequences: the node's self-update cannot download from the private repo (v0.1.0: 'no SHA256SUMS on GitHub (404: missing, or the repo is private)'), and the public download channel every install instruction points at is frozen at v0.2.186 — while runsheet/mesh-cli restarted numbering at v0.1.0, below the public line. On 2026-09-28 12:00Z the public repo was transferred: it is now github.com/runsheet/mesh (public; the old projectmesh-io/mesh URLs redirect, release downloads included). It holds a one-line README, dist/systemd and the 186 releases. Restore the public channel there, signed, same org so no cross-org token is needed: (1) release-tag.yml publishes the same signed assets (binaries, SHA256SUMS, SHA256SUMS.sigstore.json) as a release on runsheet/mesh too, same tag and notes — the workflow's GITHUB_TOKEN cannot write another repo, so use the runsheet org's App installation token or a fine-grained secret scoped to runsheet/mesh; (2) versions continue the public line: the next tag is v0.2.187, v0.1.0 is marked superseded (pre-release + a note; tag left), and node-releases-on-merge's auto-tag counts from v0.2.x; (3) the node's release Source (internal/selfupdate Source, ReleaseUpdater) downloads from runsheet/mesh — public, anonymous — while the sigstore policy keeps pinning the signing identity release-tag.yml@refs/tags/<version> of runsheet/mesh-cli (same bytes); `mesh node update` and the console's Update menu (gateway meshCliReleases: read runsheet/mesh so the console lists exactly what nodes can fetch) follow; (4) runsheet/mesh's README says what the repo is (the public release mirror of the private mesh-cli source), how to verify a download with cosign against the mesh-cli identity, and mesh-marketing's getting-started.mdx already points at runsheet/mesh; (5) the goals node-updates-from-a-private-repo and public-releases-on-the-mesh-repo were cancelled in favour of this. Amend ADR 002 (release channel) and docs/releases-after-transfer.md; mesh-specs. Live check: `setNodeDesiredVersion v0.2.187` on the mini updates it from runsheet/mesh with no by-hand copy.

## Acceptance

release-tag.yml on a v0.2.187 tag produces the signed release on runsheet/mesh-cli and an identical one on runsheet/mesh; mesh-cli tests: the updater fetches from runsheet/mesh and verifies against the runsheet/mesh-cli signing identity; the console's Update menu lists v0.2.187; live: the mini self-updates to v0.2.187 from runsheet/mesh, sessions surviving, update.json confirming; v0.1.0 is marked superseded; runsheet/mesh's README names the mirror and how to verify.

v0.2.187 was cut and signed on runsheet/mesh-cli only — its mirror step failed before it ever reached runsheet/mesh — so the live check above never ran on that tag. v0.2.188 became the first release on the mirror instead, and every clause of the acceptance above that this goal can still show live is shown against v0.2.188, not v0.2.187.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | release-tag.yml mirrors each signed release to runsheet/mesh (same tag, notes and five assets) and publishes the mirror's README; ADR 002 fixes the v0.2.x numbering | merged | mesh/signed-releases-on-runsheet-mesh-1 | runsheet/mesh-cli#315 | $3.40 |
| 2 | The node downloads releases from the public runsheet/mesh mirror and still verifies against the runsheet/mesh-cli signing identity | merged | mesh/signed-releases-on-runsheet-mesh-2 | runsheet/mesh-cli#316 | $4.01 |
| 3 | mesh-specs: the TRD's node section and ADR 007/008 name runsheet/mesh as the public release mirror nodes download from, signed by runsheet/mesh-cli | merged | mesh/signed-releases-on-runsheet-mesh-3 | runsheet/mesh-specs#75 | $1.00 |
| 4 | Cut v0.2.187 from main, mirror it to runsheet/mesh, mark v0.1.0 superseded, and update the mini from the mirror; record it in docs/releases-after-transfer.md | failed | mesh/signed-releases-on-runsheet-mesh-4 | — | $2.48 |
| 5 | Goal doc: signed-releases-on-runsheet-mesh — what landed, the mirror channel, the cancelled goals it replaces, and the runsheet half owed (meshCliReleases reading runsheet/mesh) | cancelled | — | — | — |
| 6 | Cut v0.2.187 from main, prove the mirror release on runsheet/mesh through the node's own updater, mark v0.1.0 superseded, and record it in docs/releases-after-transfer.md | failed | mesh/signed-releases-on-runsheet-mesh-6 | — | $3.13 |
| 7 | mesh-portal: ADR 005 and the node record's comments say the releases the console counts against are runsheet/mesh's, the public mirror nodes fetch from | merged | mesh/signed-releases-on-runsheet-mesh-7 | runsheet/mesh-portal#18 | $0.31 |
| 8 | mesh-specs: ADR 013's auto-tag counts from the public line on runsheet/mesh (v0.2.x), and the TRD's release paragraph links the record of v0.2.187 | merged | mesh/signed-releases-on-runsheet-mesh-8 | runsheet/mesh-specs#80 | $1.35 |
| 9 | Goal doc: signed-releases-on-runsheet-mesh — what landed, the mirror channel, v0.2.187 and the superseded v0.1.0, the cancelled goals it replaces, the mini's live check, and the runsheet half owed | cancelled | — | — | — |
| 10 | v0.2.188 is the first release on the mirror: confirm its run, prove it through the updater, annotate v0.2.187 as never mirrored, and record it in docs/releases-after-transfer.md, ADR 002 and both READMEs | merged | mesh/signed-releases-on-runsheet-mesh-10 | runsheet/mesh-cli#358 | $1.61 |
| 11 | The mini upgrades to v0.2.188 from runsheet/mesh through its own updater, sessions surviving, and docs/releases-after-transfer.md records update.json and the adopt lines | failed | mesh/signed-releases-on-runsheet-mesh-11 | — | $2.05 |
| 12 | mesh-specs: ADR 013's amendment and the TRD name v0.2.188 as the first release on runsheet/mesh, v0.2.187 as cut but never mirrored, and v0.2.189 as the first auto-cut tag | failed | mesh/signed-releases-on-runsheet-mesh-12 | — | $2.72 |
| 13 | Goal doc: signed-releases-on-runsheet-mesh — what landed, the mirror channel, v0.2.188 as the first mirrored release and v0.2.187 never mirrored, v0.1.0 superseded, the mini's bootstrap, the cancelled goals it replaces, and the runsheet half owed | cancelled | — | — | — |
| 14 | The record is true: v0.2.188 is on runsheet/mesh (attempt 3 of run 36499785802) and the mini runs it from the mirror; docs/releases-after-transfer.md, ADR 002, both READMEs and v0.1.0's note say so | merged | mesh/signed-releases-on-runsheet-mesh-14 | runsheet/mesh-cli#385 | $2.88 |
| 15 | mesh-specs: ADR 013's amendment and the TRD name v0.2.188 as the first release on runsheet/mesh, v0.2.187 as cut but never mirrored, and v0.2.189 as the first auto-cut tag | merged | mesh/signed-releases-on-runsheet-mesh-15 | runsheet/mesh-specs#93 | $1.24 |
| 16 | Goal doc: signed-releases-on-runsheet-mesh — what landed, the mirror channel, v0.2.188 as the first mirrored release and v0.2.187 never mirrored, v0.1.0 superseded, the mini's self-update, the cancelled goals it replaces, and what is owed to runsheet | cancelled | mesh/signed-releases-on-runsheet-mesh-16 | — | $1.48 |
| 17 | The record of v0.2.188 on the mirror and the mini's self-update is true and merges: docs/releases-after-transfer.md, ADR 002, both READMEs and .runsheet/validate.yml (carry PR #385 forward) | cancelled | mesh/signed-releases-on-runsheet-mesh-17 | — | $3.12 |
| 18 | mesh-specs: ADR 013's amendment and the TRD name v0.2.188 as the first release on the mirror, v0.2.187 as cut but never mirrored, and v0.2.189 as the first auto-cut tag (carry PR #93 forward) | failed | mesh/signed-releases-on-runsheet-mesh-18 | — | $1.82 |
| 19 | Goal doc: signed-releases-on-runsheet-mesh — what landed, the mirror channel, v0.2.188 first on the mirror and v0.2.187 never mirrored, v0.1.0 superseded, the mini's self-update, the cancelled goals it replaces, and what is owed to runsheet | cancelled | — | — | — |
| 20 | Goal doc: signed-releases-on-runsheet-mesh — what landed, v0.2.188 first on the mirror and v0.2.187 never mirrored, v0.1.0 superseded, the mini's upgrade as the record shows it, the cancelled goals it replaces, and what is owed | running | mesh/signed-releases-on-runsheet-mesh-20 | — | — |
| 21 | docs/releases-after-transfer.md: a dated addendum of the mirror channel's state on 2026-09-29 — desiredVersion, update.json, the mirror README's lag, and the auto-tag blocked on RELEASE_TAG_TOKEN | merged | mesh/signed-releases-on-runsheet-mesh-21 | runsheet/mesh-cli#390 | $3.04 |

## What landed

**Item 1: release-tag.yml mirrors releases to runsheet/mesh** (runsheet/mesh-cli#315). Changed `.github/workflows/pr-checks.yml`, `.github/workflows/release-tag.yml`, `README.md`, `dist/mirror/README.md`, `docs/adr/adr-002-signed-tag-releases.md`, `scripts/mirror-release.sh` and `scripts/mirror-release_test.sh`. `release-tag.yml`'s `publish` job computes the release notes once, creates the release on `runsheet/mesh-cli` with `--verify-tag`, then `scripts/mirror-release.sh` creates the identical release on `runsheet/mesh` — same tag, notes and five assets — proves the mirror's `SHA256SUMS` byte-identical and still verifying against the `runsheet/mesh-cli` identity, and pushes `dist/mirror/README.md` to the mirror's `README.md` only when it differs.

**Item 2: the node downloads from runsheet/mesh** (runsheet/mesh-cli#316). Changed `README.md`, `docs/adr/adr-002-signed-tag-releases.md`, `internal/commands/nodeupdate.go`, `internal/commands/nodeupgrade.go`, `internal/node/selfupdate.go`, `internal/selfupdate/errors.go`, `internal/selfupdate/fetch.go`, `internal/selfupdate/fetch_test.go`, `internal/selfupdate/latest.go`, `internal/selfupdate/latest_test.go`, `internal/selfupdate/reference_test.go` and `internal/selfupdate/verify_test.go`. `ReleaseBaseURL` and `LatestReleaseURL` now name `runsheet/mesh`: the node, `mesh node update` and `mesh node upgrade` fetch anonymously from the public mirror while the signing identity stays `runsheet/mesh-cli`'s — the mirror holds the same bytes that repo's `release-tag.yml` signed.

**Item 3: mesh-specs names the mirror** (runsheet/mesh-specs#75). Changed `specs/adr-007-sessions-in-sandboxes.md`, `specs/adr-008-sessions-outlive-the-node.md` and `specs/trd.md`: the TRD's node section and ADRs 007 and 008 name `runsheet/mesh` as the public release mirror a node's self-update downloads from, while the signature it verifies stays the `runsheet/mesh-cli` identity. (The file list is the platform's record of the pull request; this session's token is scoped to mesh-cli alone, adr-001-item-scoped-github-token.)

**Item 7: mesh-portal names the mirror** (runsheet/mesh-portal#18). Changed `docs/adr/adr-005-node-releases-on-the-console.md` (the platform's record of the pull request's documents): the console's ADR on node releases names `runsheet/mesh` as the release line a node's version is measured against. This session's token reaches mesh-cli alone, so any non-document file the pull request also touched is not listed here.

**Item 8: mesh-specs' auto-tag counts from the mirror** (runsheet/mesh-specs#80). Changed `specs/adr-013-node-releases-on-merge.md` and `specs/trd.md` (the platform's record of the pull request): ADR 013's amendment has the auto-tag count from the public `v0.2.x` line on `runsheet/mesh`, and the TRD's release paragraph links the record of v0.2.187.

**Item 10: v0.2.188 is the first release on the mirror** (runsheet/mesh-cli#358). Changed `README.md`, `dist/mirror/README.md`, `docs/adr/adr-002-signed-tag-releases.md` and `docs/releases-after-transfer.md`. Confirmed run 36499785802's attempt 3 as the first signed release on `runsheet/mesh`, annotated v0.2.187 as never mirrored (run 36494313388's mirror step failing before `--target` was added by #336), and recorded both in ADR 002 and the two READMEs.

**Item 14: the record matches the live repos** (runsheet/mesh-cli#385). Changed `.runsheet/validate.yml`, `README.md`, `dist/mirror/README.md`, `docs/adr/adr-002-signed-tag-releases.md` and `docs/releases-after-transfer.md`. Re-verified that v0.2.188 is on `runsheet/mesh` (attempt 3 of run 36499785802) and that the mini runs it from the mirror, and updated v0.1.0's own release note to name v0.2.188 rather than the still-unmirrored v0.2.187.

**Item 15: mesh-specs' amendment matches the record** (runsheet/mesh-specs#93). Confirmed changed: `specs/adr-013-node-releases-on-merge.md` and `specs/trd.md` — the same two files item 8 touched. ADR 013's amendment now names v0.2.188 as the first release on the mirror, v0.2.187 as cut but never mirrored, and v0.2.189 as the first tag the auto-cut rule names next.

**Item 21: the 2026-09-29 addendum** (runsheet/mesh-cli#390, merged 2026-09-29 03:10Z as `1a57ab0`). Changed `docs/releases-after-transfer.md` alone: a dated addendum recording the channel's state on 2026-09-29 — the platform's `desiredVersion` for the mini at v0.2.188, `update.json` still holding the v0.1.0 refusal, the live `runsheet/mesh` README lagging `dist/mirror/README.md`, and the auto-tag blocked on `RELEASE_TAG_TOKEN`.

## The release line

- **v0.2.1–v0.2.186**: unsigned, built by the old `cli-release.yml` on every push to `main`, published on the public repo before the transfer; the last, v0.2.186, at 27e5c88 on 2026-09-26 11:48Z.
- **v0.1.0**: cut 2026-09-28 on the private `runsheet/mesh-cli` after the cross-org channel was dropped, restarting numbering below the public line. It is now a superseded pre-release (its note points at the v0.2.x line; its tag is left as it was).
- **v0.2.187**: cut and signed on `runsheet/mesh-cli` only, 2026-09-28 22:46Z (`909202b`). Its mirror step (run 36494313388) failed before it reached `runsheet/mesh`: `gh release create` refused with "tag v0.2.187 exists locally but has not been pushed to runsheet/mesh … specify the `--target` flag", because the mirror had no commit of ours to create the tag on. #336 fixed it with `--target main`. v0.2.187 has never reached the mirror.
- **v0.2.188**: the first release on the mirror (run 36499785802, `5a8c29e`, the merge of #336). Attempt 1 (2026-09-29 00:03Z) signed and released it on `runsheet/mesh-cli`, then the mirror refused it with `HTTP 403: Resource not accessible by personal access token`, because `RELEASE_TOKEN` was a fine-grained PAT whose resource owner had not followed the repository to `runsheet`. Attempt 2 (00:48Z) failed one step earlier because the source release from attempt 1 still existed. Attempt 3 (00:50Z), after the operator replaced `RELEASE_TOKEN` with one owned by `runsheet` and scoped to `runsheet/mesh` alone, succeeded on both repositories; the mini has run it since 00:51:59Z.
- **v0.2.189**: the next tag under ADR 002's rule (the highest `v0.2.x` on the mirror plus one), not yet cut. `release-on-merge.yml`'s tag job (run 36507379094, on the merge of #320, 2026-09-29 01:19:49Z) failed at its own guard clause before it computed a version or pushed anything: `RELEASE_TAG_TOKEN is not set`. That secret — a fine-grained token with Contents: read and write on `runsheet/mesh-cli`, distinct from `GITHUB_TOKEN` because a `GITHUB_TOKEN` push starts no workflow — belongs to goal `node-releases-on-merge`, not this one, and is still unset.

## The mini's upgrade

Recorded in full in [docs/releases-after-transfer.md](../docs/releases-after-transfer.md), "The mini updates from runsheet/mesh". The mini runs v0.2.188, byte-identical to the mirror's signed `mesh-linux-amd64` (`sha256sum` matches the release's `SHA256SUMS`). The update was recorded as pending (`confirming update 4d8d435 → v0.2.188: start 1 of 3`) and confirmed by the first heartbeat 20 seconds later (`self-update: v0.2.188 is up; update confirmed`). Two planner sessions running in their own units (pids 1395781 and 1395870, started at 00:50:27 and 00:50:33) survived the restart and were adopted by the new process at 00:52:06.

**How it was triggered.** It was the operator running `mesh node upgrade` over ssh — two ssh logins at 00:51:39 and 00:51:59, with `~/bin/mesh.new.sums` left at 00:51:45 byte-identical to the mirror's `SHA256SUMS`, and a `systemctl --user restart` at the same second the new process started (the restart-and-adopt path, not a drain). The evidence cannot tell `mesh node upgrade --version v0.2.188` (fetching and verifying from the mirror through `internal/selfupdate`) apart from `mesh node upgrade --binary ~/bin/mesh.new` (swapping in a file the operator fetched from the mirror separately) — no `~/bin/mesh.new` was left behind, and neither is claimed here. Either way, the bytes are the mirror's signed binary.

**The desiredVersion-driven loop path was not exercised for v0.2.188.** Between 00:40 and 00:51:59 the loop's only `self-update:` lines are for `v0.1.0` — the platform's `desiredVersion` for the mini at the time — fetched and refused by the running `4d8d435` binary (no `SHA256SUMS` for `v0.1.0`, first on GitHub while the repo was private, then on `runsheet/mesh` since it was never published there either). The loop never fetched or asked for v0.2.188 on its own; only the operator's by-hand upgrade put it on the machine. `update.json` still holds that v0.1.0 refusal, not a v0.2.188 one:

```json
{
  "from": "v0.2.188",
  "to": "v0.1.0",
  "attempts": 0,
  "lastError": "release v0.1.0 has no SHA256SUMS on runsheet/mesh (404: not published there, or not a signed release)"
}
```

`Confirm` cleared v0.2.188's own pending state at 00:52:19; the record above is the *next* attempt's refusal, timestamped 00:52:20, after the loop read the platform's `desiredVersion` again. Item 21's 2026-09-29 addendum re-read the platform live and found `desiredVersion` now at `v0.2.188` — moved by the operator sometime after this record, not by any node session — so the loop has had nothing to fetch since, and `update.json`'s stale `lastError` is exactly that: a stale refusal sitting between attempts, not a live condition.

## Goals this replaces

- **node-updates-from-a-private-repo** — cancelled 2026-09-28 in favour of this goal. The node's self-update could not work from `runsheet/mesh-cli` while that repo stayed private; this goal moves the public download source to the mirror instead of making the source public.
- **public-releases-on-the-mesh-repo** — cancelled 2026-09-28 in favour of this goal. The old unsigned releases on the pre-transfer public repo needed replacing with signed ones; this goal restores that channel on `runsheet/mesh` after the transfer, signed, rather than re-opening the old cross-org unsigned one.

## Owed elsewhere

1. **`meshCliReleases` reading `runsheet/mesh`** — owed on `runsheet/runsheet`. The gateway query behind the console's Update menu must read the public mirror, so the menu lists exactly what a node can fetch (ADR 013's "What is owed to runsheet/runsheet"). The mini's `desiredVersion` is set by the operator through `setNodeDesiredVersion` until then.
2. **`RELEASE_TAG_TOKEN`** — owed on `runsheet/mesh-cli` as a repository secret (goal `node-releases-on-merge`). Unset today: run 36507379094 failed at its guard clause before computing or pushing a version, so no `v0.2.189` — or any tag after it — has been cut.
3. **The live mirror README still says "From v0.2.187"** — it reads what `main`'s `dist/mirror/README.md` held at the commit v0.2.188's mirror step ran from. Nobody edits it by hand; only the *next* release's mirror step pushes `main`'s current copy, which already reads "From v0.2.188". It will correct itself once v0.2.189 (or any later tag) is cut and mirrored — which is itself blocked on item 2 above.
4. **v0.2.187's release notes** — its "Never mirrored" paragraph's second sentence still calls v0.2.188 unmirrored and v0.2.189 the retry, both now stale. Editing it is the operator's call; no item in this goal has touched the v0.2.187 release itself.
5. **mesh-marketing's getting-started.mdx** — already points at `runsheet/mesh`; nothing owed there.

The record is [docs/releases-after-transfer.md](../docs/releases-after-transfer.md); the design is [ADR 002](../docs/adr/adr-002-signed-tag-releases.md) ("Amendment: the public mirror on runsheet/mesh" and "Amendment: the node downloads from runsheet/mesh") and mesh-specs' [ADR 013 amendment](https://github.com/runsheet/mesh-specs/blob/main/specs/adr-013-node-releases-on-merge.md) ("Amendment: the version line is the public mirror's").

## Log

- 2026-09-28 12:00Z · the public repo transferred to github.com/runsheet/mesh
- 2026-09-28 · item 1 started and merged: release-tag.yml mirrors releases to runsheet/mesh, ADR 002 fixes the numbering (runsheet/mesh-cli#315)
- 2026-09-28 · item 2 started and merged: the node downloads from runsheet/mesh, verifying against the runsheet/mesh-cli identity (runsheet/mesh-cli#316)
- 2026-09-28 · item 3 started and merged: mesh-specs names runsheet/mesh as the public mirror (runsheet/mesh-specs#75)
- 2026-09-28 22:46Z · v0.2.187 cut and signed on runsheet/mesh-cli only (909202b); its mirror step (run 36494313388) failed without --target
- 2026-09-28 · item 4 started and failed: could not cut and mirror v0.2.187 or update the mini, because item 2's changes were not yet on main when it ran
- 2026-09-28 · item 5 started and cancelled: an early goal-doc attempt, superseded before it produced a diff
- 2026-09-28 23:28Z · v0.2.187's release published on runsheet/mesh-cli (never mirrored)
- 2026-09-28 · item 6 started and failed: recorded v0.2.187's cut and mirror failure correctly, but `mesh node update --verify-only v0.2.187` still exits 1 against the mirror, which the item's acceptance required to exit 0
- 2026-09-28 · item 7 started and merged: mesh-portal names runsheet/mesh as what the console counts a node's releases against (runsheet/mesh-portal#18)
- 2026-09-28 · item 8 started and merged: mesh-specs' auto-tag counts from the public line on runsheet/mesh (runsheet/mesh-specs#80)
- 2026-09-28 · item 9 started and cancelled: a second goal-doc attempt, superseded before it produced a diff
- 2026-09-29 00:03Z · run 36499785802 attempt 1: v0.2.188 signed on runsheet/mesh-cli; the mirror step refused it with HTTP 403 from RELEASE_TOKEN
- 2026-09-29 00:48Z · run 36499785802 attempt 2: failed one step earlier because the source release from attempt 1 still existed
- 2026-09-29 00:50Z · run 36499785802 attempt 3: v0.2.188 signed and released on both runsheet/mesh-cli and runsheet/mesh, after the operator replaced RELEASE_TOKEN with one owned by runsheet and scoped to runsheet/mesh alone
- 2026-09-29 00:51:59Z · the mini upgrades to v0.2.188 through the operator's `mesh node upgrade`; two planner sessions (pids 1395781, 1395870) adopted at 00:52:06; the update confirmed at 00:52:19
- 2026-09-29 00:58Z · item 10 started and merged: v0.2.188 confirmed as the first release on the mirror, v0.2.187 annotated as never mirrored (runsheet/mesh-cli#358)
- 2026-09-29 · item 11 started and failed: the mini's own update.json still showed `to: v0.1.0` with no confirmedAt when this item ran, so its claim that the mini had upgraded to v0.2.188 through its own updater did not hold
- 2026-09-29 · item 12 started and failed: its ADR/TRD text called v0.2.188 the first release on the mirror before the merged record on runsheet/mesh-cli agreed, and separately mis-stated v0.2.189 as the numbering rule's next name while the mirror was still at v0.2.186
- 2026-09-29 · item 13 started and cancelled: a third goal-doc attempt, superseded before it produced a diff
- 2026-09-29 02:09Z · item 14 started and merged: re-verified v0.2.188 live on runsheet/mesh and the mini running it; v0.1.0's own release note updated (runsheet/mesh-cli#385)
- 2026-09-29 · item 15 started and merged: mesh-specs' ADR 013 amendment and the TRD brought into line with the merged record (runsheet/mesh-specs#93)
- 2026-09-29 · item 16 started and cancelled: a fourth goal-doc attempt; its validation attestation failed to upload (unauthenticated) after the item had already spent against it
- 2026-09-29 · item 17 started and cancelled: carried PR #385 forward against a stale base; its validation attestation likewise failed to upload (unauthenticated)
- 2026-09-29 · item 18 started and failed: carried mesh-specs PR #93 forward, but two of its grep checks did not match the wording its own acceptance named
- 2026-09-29 · item 19 started and cancelled: a fifth goal-doc attempt, superseded before it produced a diff
- 2026-09-29 01:19:49Z · run 36507379094 (release-on-merge.yml, merge of #320) fails at its guard clause: RELEASE_TAG_TOKEN is not set, so v0.2.189 is not cut
- 2026-09-29 · item 20 (this item) started: writes this document from the merged record on mesh-cli, mesh-specs and mesh-portal, and from docs/releases-after-transfer.md's 2026-09-29 addendum
- 2026-09-29 · item 21 started; merged 03:10Z as runsheet/mesh-cli#390 (`1a57ab0`): a dated addendum confirming desiredVersion, the stale update.json, the live README's lag and the auto-tag still blocked on RELEASE_TAG_TOKEN
