---
title: Goal — Every node-changing merge to mesh-cli main is a release the console offers, and a node can follow the latest by itself
kind: plan
status: running
parent: prd
summary: A merge touching cmd/ or internal/ tags a release automatically (release-on-merge.yml → release-tag.yml) and the console shows a node's commit and how many releases behind it is; the first auto-cut tag, v0.2.189, waits on RELEASE_TAG_TOKEN, which is Chris's to add — everything else here is built and merged.
---

# Goal — Every node-changing merge to mesh-cli main is a release the console offers, and a node can follow the latest by itself

Mesh goal `node-releases-on-merge`, running: items 1, 2, 4, 5, 7, 12 and 13
merged; items 3, 9 and 10 failed; items 6 and 8 cancelled (item 8's PR
carried forward by item 12); item 11 pending; this document is item 14.

## Statement

Chris, 2026-09-28: the mini was found running mesh-cli `72ff910` while main
was ten commits ahead (pitches, Codex evaluations, MCP call counts,
tool-result trimming) — "are we on an old version of mesh-cli?". mesh-cli had
no deploy: merged node goals did nothing until a binary reached the mini,
and the only way there was the by-hand upgrade that is now only the fallback
(HANDOFF 2026-09-26 08:35Z), and a by-hand upgrade rolled back that same
morning under load. The proper path existed and was unused:
`update-nodes-from-console` + `node-self-update` let the console offer
GitHub releases of runsheet/mesh-cli and the node update itself to the
version the platform asks for (`Node.desired_version`) — but the only
releases were two `v0.0.0-*-test` pre-releases until `v0.1.0` was cut that
day. This goal closes the gap: a node-changing merge tags itself, the
console shows what a node runs and how far behind it is, and an org can let
the platform drive the update with no person.

## Acceptance

A merge touching `cmd/` or `internal/` on mesh-cli main produces a tagged
release with signed binaries within 15 minutes and release notes grouped by
goal; a docs-only merge produces none (workflow tests or a dry run in the
PR); the console's Nodes page offers the new release and flags the mini as
behind until it updates; with `nodes.autoUpdate` on, the mini updates itself
from a release with no person and its sessions survive (e2e with the fake
GitHub for the platform half; live check on the mini). HANDOFF no longer
describes the by-hand path except as the fallback.

## Items

| Seq | Title | Status | Branch | PR | Cost | Model |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | release-on-merge: a node-changing merge to main bumps the version, tags, and release-tag.yml builds it; a docs-only merge cuts none | merged | mesh/node-releases-on-merge-1 | https://github.com/runsheet/mesh-cli/pull/330 | $1.53 | opus |
| 2 | release-tag.yml release notes: the merged PR titles since the last release, grouped by goal slug with the goal-doc link | merged | mesh/node-releases-on-merge-2 | https://github.com/runsheet/mesh-cli/pull/317 | $2.28 | sonnet |
| 3 | Node: the reported version is the release tag when built from one, the commit sha otherwise, and the heartbeat carries `commit` | failed (out of budget, no PR) | mesh/node-releases-on-merge-3 | — | $4.05 | sonnet |
| 4 | Console: the Nodes list and node page show the release tag and commit, and flag a node behind on releases | merged | mesh/node-releases-on-merge-4 | https://github.com/runsheet/mesh-portal/pull/16 (amended by https://github.com/runsheet/mesh-portal/pull/18, releases counted against runsheet/mesh) | $0.84 | sonnet |
| 5 | mesh-specs: TRD names the release path, `commit`, `behind`, and `nodes.autoUpdate` | merged | mesh/node-releases-on-merge-5 | https://github.com/runsheet/mesh-specs/pull/76 (amended by https://github.com/runsheet/mesh-specs/pull/80 and https://github.com/runsheet/mesh-specs/pull/93) | $1.37 | sonnet |
| 6 | Goal doc: node-releases-on-merge — first attempt | cancelled | — | — | $0.00 | haiku |
| 7 | release-on-merge: a node-changing merge to main bumps the version, tags, and release-tag.yml builds it; a docs-only merge cuts none (retry, evaluation on the first attempt failed) | merged | mesh/node-releases-on-merge-7 | https://github.com/runsheet/mesh-cli/pull/337 | $0.85 | sonnet |
| 8 | Node: the reported version is the release tag when built from one, the commit sha otherwise, and the heartbeat carries `commit` (retry of item 3) | cancelled — PR closed unmerged, carried forward by item 12 | mesh/node-releases-on-merge-8 | https://github.com/runsheet/mesh-cli/pull/338 (closed, not merged) | $7.53 | opus |
| 9 | Goal doc: node-releases-on-merge — second attempt | failed (three evaluations, no PR) | mesh/node-releases-on-merge-9 | — | $1.90 | opus (planned haiku, sharpened) |
| 10 | mesh-cli README and docs/rollout.md: a merge is a release and the mini follows it (first attempt) | failed (GitHub rate limit) | — | — | $0.00 | sonnet |
| 11 | Goal doc: node-releases-on-merge — third attempt | pending | — | — | $0.00 | sonnet |
| 12 | Node: the reported version is the release tag when built from one, the commit sha otherwise, and the heartbeat carries `commit` when the gateway takes it (carry PR #338 forward onto main) | merged | mesh/node-releases-on-merge-12 | https://github.com/runsheet/mesh-cli/pull/402 | $1.97 | opus |
| 13 | mesh-cli README and docs/rollout.md: a merge is a release and the mini follows it; the by-hand `--binary` upgrade is the fallback, not the way | merged | mesh/node-releases-on-merge-13 | https://github.com/runsheet/mesh-cli/pull/401 | $3.00 | opus (planned sonnet, sharpened) |
| 14 | Goal doc: node-releases-on-merge — every item's PR and status as Mesh records them, the first automatic release blocked on RELEASE_TAG_TOKEN, the mini's path, and what is owed to runsheet and to Chris | running | mesh/node-releases-on-merge-14 | — | — | sonnet |

Every PR above is confirmed `MERGED` by `gh pr view --json state` at the time
this document was written, except #338, which is `CLOSED` (never merged) and
carried forward by item 12's PR #402. Item 12's PR merged at
2026-09-29T07:49:11Z, shortly before this item started; an earlier attempt at
this document was rejected in part because #402 still showed as unmerged
when it read the record.

## What landed

**Item 1 — `release-on-merge.yml` (mesh-cli#330; superseded in shape by #337, item 7).**
The workflow runs on push to `main`, diffs the merge commit against its
parent, and cuts nothing when every changed path is outside `cmd/`,
`internal/`, `images/node/`, `go.mod` and `go.sum`. Otherwise
`scripts/release-on-merge.sh` reads the latest `v*` tag, bumps the patch —
the minor instead when the merging PR carries the `release:minor` label or a
`specs/goal-*.md` the merge touches says `release: minor` — and pushes the
new tag with the repository secret `RELEASE_TAG_TOKEN`, never `GITHUB_TOKEN`
(whose push starts no workflow, so `release-tag.yml` would never fire).

**Item 2 — release notes by goal (mesh-cli#317).**
`release-tag.yml` builds the release body itself from `git log` between the
previous tag and this one: the merged PR titles, grouped by the goal slug
each title names (the `adr-003-plan-from-the-truth` convention), each group
linking the goal's `specs/goal-<slug>.md` when one exists on `main`; titles
naming no goal are listed last, ungrouped.

**Item 4 — the console shows commit and behind (mesh-portal#16, amended by #18).**
`NodeRow` carries optional `commit` and `behind { releases, latest }`. The
Nodes list shows the version (tag or sha) with the commit appended only when
it differs, and a badge only when `behind` is present — a warning badge for
more than one release behind, muted for exactly one, nothing for zero. The
node page repeats this on its Version and Behind stats. #18 amended ADR 005
and the node record's comments to say the releases counted are
`runsheet/mesh`'s — the public mirror, the same signed bytes — not
`runsheet/mesh-cli`'s private tag list, so the console never names a tag a
node cannot fetch.

**Item 5 — the TRD names the contract (mesh-specs#76, amended by #80 and #93).**
`trd` §1 Node gained `commit` and `behind`, and §3 gained the release path
and `nodes.autoUpdate`. #80 amended ADR 013 and the TRD so the auto-tag
counts from the public line on `runsheet/mesh` (`v0.2.x`), never from
`v0.1.x` or `v0.0.0` test tags. #93 amended ADR 013 again to name `v0.2.188`
as the first signed release on the mirror and the version line it counts
from.

**Item 7 — `release-on-merge.yml`, corrected (mesh-cli#337).**
Item 1's evaluation failed on its diff filter and its dry-run test coverage;
item 7 is the retry that merged in its place with the same shape described
above.

**Item 12 — the node reports tag-or-sha and `commit` (mesh-cli#402, carrying #338 forward).**
`internal/version.Resolved()` returns the release tag when the binary was
built from one, the short sha otherwise; `HeartbeatExtras.commit` carries the
sha separately — even when `version` is already a tag — because a patch tag
and its predecessor can share a binary when nothing in the tagged paths
changed. `mesh node status`'s standalone heartbeat carries `commit` beside
`version` too. Item 8's PR #338 closed unmerged (its evaluation could not be
completed in budget); item 12 redid the same work and merged.

**Item 13 — README and docs/rollout.md say a merge is the way (mesh-cli#401).**
`docs/rollout.md`'s "Node deploys" section (added by this item) states the
release path — merge, tag, signed build, mirror, console offer,
`nodes.autoUpdate` once it lands — and names the `RELEASE_TAG_TOKEN`
prerequisite and run `36507379094`'s failure at that guard. The by-hand
`mesh node upgrade --binary` and the draining `mesh node update <tag>` are
kept as the documented fallback, not the ordinary path. The README carries
the matching update.

**Items 3, 6, 8, 9, 10, 11 — what did not land.** Item 3 (the tag-or-sha
report) ran out of budget before an evaluator could pass it; item 8 retried
it and its PR #338 closed unmerged for the same reason; item 12 is the
version of that work that merged. Item 6, the first goal-doc attempt, was
cancelled before it ran. Item 9, the second goal-doc attempt, failed three
evaluations in a row (exactness against Mesh's own PR records was the
recurring complaint) and left no PR. Item 10, the first attempt at the
README/rollout.md item, failed on a GitHub API rate limit with no PR; item
13 is its retry, and it merged. Item 11, the third goal-doc attempt, is
pending in Mesh's record and has not run; this document, item 14, is the
attempt made once items 12 and 13 had merged.

## The first automatic release

No merge has been auto-tagged yet. `release-on-merge.yml` run **36507379094**
(triggered by the merge of #396, "The built-in context tool is called
mesh-context…", 2026-09-29T01:19:49Z) ran to its "Tag the merge" job and
stopped at its own guard clause before computing a version or pushing
anything:

```
RELEASE_TAG_TOKEN is not set. Create a fine-grained token with Contents:
read and write on runsheet/mesh-cli and add it as the repository secret
RELEASE_TAG_TOKEN. GITHUB_TOKEN cannot be used: a tag it pushes starts no
workflow, so release-tag.yml would never run
(docs/adr/adr-002-signed-tag-releases.md).
```

See `docs/releases-after-transfer.md`'s 2026-09-29 addendum for the full
trace. The public line today is **v0.2.188** on the mirror `runsheet/mesh`
(the first signed release there, mesh-specs ADR 013's amendment) —
`runsheet/mesh-cli` itself also carries an unmirrored `v0.2.187`, which is
not on the line the auto-tag counts from, since `release-on-merge` reads
`runsheet/mesh`'s tags only. Once `RELEASE_TAG_TOKEN` exists, the next
node-changing merge's `release-on-merge.yml` run bumps past `v0.2.188` to
**v0.2.189** — the first tag this pipeline cuts on its own — and
`release-tag.yml` builds, signs and mirrors it exactly as it does for a tag
pushed by hand. Until then there is no cross-build on the ordinary path: the
fallback, a cross-build and `mesh node upgrade --binary` by hand (as
`docs/rollout.md`'s "Node deploys" section names it), is the only interim
way to put a node-changing commit onto the mini ahead of a release.

## The mini

The mini runs **v0.2.188**, fetched from the public mirror `runsheet/mesh`
and verified against `runsheet/mesh-cli`'s `release-tag.yml` signing
identity (`docs/releases-after-transfer.md`). Until `nodes.autoUpdate`
exists and is turned on, nothing moves it further by itself: a person either
sets `desiredVersion` on the console, or runs `mesh node update <tag>` on
the mini directly — both take a signed release, verify it, and drain the
node's sessions before swapping (the draining variant, unlike an
adopting `--binary` upgrade). The `--binary` upgrade is only the fallback,
for a build the release pipeline has not tagged: the fallback builds, `scp`s
the binary and runs `mesh node upgrade --binary <path>` without a drain, as
`docs/rollout.md`'s "Node deploys" section (landed by item 13) describes —
never the ordinary way to reach the mini.

## Owed to runsheet/runsheet

Everything below is the platform's half; nothing in this repo blocks it.

- **`nodes.autoUpdate`** (org setting, default off): its storage, its console
  toggle, and a tick that sets `desired_version` to the latest non-prerelease
  `runsheet/mesh` release for an idle, out-of-date node — skipping any node
  whose latest heartbeat carries `updateError` — at most once an hour per
  node, with the confirm window from `upgrades-survive-a-slow-platform` so a
  slow platform heartbeat does not roll a good update back.
- **`behind` measured in releases**, not just commits: `mesh_health_nodes`
  (`node-behind-main-is-seen` already flags commits behind main; this adds
  the same flag counted in releases of `runsheet/mesh`) and the console's
  Nodes page, whose reading side (`commit`, `behind`) already shipped in
  mesh-portal ADR 005 (item 4/#16, amended by #18).
- **The Update button defaulting to the latest** release on the node page,
  so an operator does not have to know the tag to pick it.
- **`HeartbeatNodeInput.commit`**, the gateway field the node's `commit`
  extra is sent for once introspection finds it, so the sha rides the node
  record and shows on the console beside the version tag.
- **HANDOFF's "Mesh node deploys" section** no longer saying by hand: once
  the platform half above lands, this goal's closing item updates it to
  describe the merge-to-release path as the way and the by-hand upgrade as
  the fallback, matching `docs/rollout.md`'s "Node deploys" section here.

## Owed to Chris

- **Add `RELEASE_TAG_TOKEN`** as a repository secret on `runsheet/mesh-cli`:
  a fine-grained token scoped to `Contents: read and write` on that one
  repository (README, "Setting up releases"). `GITHUB_TOKEN` cannot stand in
  for it — GitHub refuses to let a workflow's default token trigger another
  workflow, so a tag it pushed would never reach `release-tag.yml`. Once the
  secret exists, the next node-changing merge's `release-on-merge.yml` run
  succeeds where `36507379094` stopped, and cuts **v0.2.189**.

Related: mesh-specs [ADR 013](https://github.com/runsheet/mesh-specs/blob/main/specs/adr-013-node-releases-on-merge.md)
("A node-changing merge is a release, and a node follows the latest by
itself"), mesh-portal [ADR 005](https://github.com/runsheet/mesh-portal/blob/main/docs/adr/adr-005-node-releases-on-the-console.md)
("Node releases on the console"), mesh-cli
[ADR 002](docs/adr/adr-002-signed-tag-releases.md) ("Signed tag releases").

## Log

- 2026-09-28 · Chris finds the mini ten commits behind main and asks whether
  it is running an old version; goal `node-releases-on-merge` is set.
- 2026-09-28/29 · items 1 (#330), 2 (#317), 4 (mesh-portal#16), 5
  (mesh-specs#76) merge; item 3 runs out of budget with no PR.
- 2026-09-29 · item 7 (#337) merges, correcting item 1's evaluation failure;
  item 8 (#338) closes unmerged, also out of budget; mesh-portal#18 and
  mesh-specs#80 and #93 amend items 4 and 5's ADRs for the public mirror's
  numbering.
- 2026-09-29 · goal-doc attempt 6 is cancelled and 9 fails
  evaluation (three failures on item 9 alone, mostly on exactness against
  Mesh's PR records); item 10, the first README/rollout.md attempt, fails on
  a GitHub rate limit.
- 2026-09-29 01:19Z · `release-on-merge.yml` run `36507379094` fires on the
  merge of #396 and stops at its `RELEASE_TAG_TOKEN` guard — the first real
  test of the pipeline this goal built, and the first automatic release is
  still pending that secret.
- 2026-09-29 · item 12 (#402) merges the tag-or-sha report, carrying item
  8's #338 forward; item 13 (#401) merges the README and
  `docs/rollout.md` "Node deploys" section.
- 2026-09-29 07:49Z · item 12's PR #402 merges; this document, item 14,
  starts minutes later and records every item's PR by `gh pr view`'s own
  answer rather than repeating an earlier attempt's stale read.
