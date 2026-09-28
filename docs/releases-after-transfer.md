---
summary: The first tags cut from runsheet/mesh-cli's main after the transfer — v0.0.0-runsheet-test.6 (its release, its signed node image under ghcr.io/runsheet, `mesh node update --verify-only` accepting it) and v0.2.187, the first signed v0.2.x release, whose mirror to runsheet/mesh failed on a mirror-release.sh bug while v0.1.0 was marked superseded.
---

# Releases after the transfer

Recorded 2026-09-26 for item 8 of goal `mesh-cli-lives-in-runsheet`. It
follows the `v0.0.0-selfupdate-test.1` verification (PR #168) and
checks the contract in ADR 002's transfer amendment. The tag is cut
from `main` after #206 landed the `release-tag.yml` and Containerfile
fixes ("Transfer fixes" in ADR 002). It replaces item 6's draft, which
verified a tag cut from a branch.

## Tag

`v0.0.0-runsheet-test.6` points at `184011c`, which was `origin/main`'s
head when it was pushed: the merge of #206.

```
$ git rev-list -n1 v0.0.0-runsheet-test.6
184011c99d43df6057e92321ff096998d87dfbd5
$ git merge-base --is-ancestor 184011c99d43df6057e92321ff096998d87dfbd5 origin/main; echo exit=$?
exit=0

$ gh run list --repo runsheet/mesh-cli --workflow release-tag.yml -L 3
completed	success	Everything that named projectmesh-io/mesh-cli names runsheet/mesh-cli…	Release (tag)	v0.0.0-runsheet-test.6	push	36249480345	22m35s	2026-09-26T14:43:35Z
completed	success	Release (tag)	Release (tag)	v0.0.0-runsheet-test.5	push	36247714213	21m15s	2026-09-26T14:12:26Z
completed	failure	Release (tag)	Release (tag)	v0.0.0-runsheet-test.4	push	36245651812	21m10s	2026-09-26T13:35:03Z

$ gh run view 36249480345 --repo runsheet/mesh-cli --json jobs -q '.jobs[]|"\(.name)\t\(.conclusion)"'
Build linux/amd64	success
Node image v0.0.0-runsheet-test.6	success
Build linux/arm64	success
Build darwin/arm64	success
Sign and publish v0.0.0-runsheet-test.6	success
```

The build jobs check out the tag's commit:
`HEAD is now at 184011c Everything that named projectmesh-io/mesh-cli names runsheet/mesh-cli: … (#206)`.

**The stale test tags are gone.** `v0.0.0-runsheet-test.1` to `.5` were
cut while the fixes were still on item 6's branch, or before they
merged. None of them is on the remote now, as a tag or as a release, so
this session had nothing to delete. `v0.0.0-runsheet-test.6` and the
pre-transfer `v0.0.0-selfupdate-test.1` are the only releases left:

```
$ git ls-remote --tags origin 'v0.0.0-runsheet-test.*'
ec6d1fce1a79a12d3abd97d97e1fbc4dec9a6e83	refs/tags/v0.0.0-runsheet-test.6
184011c99d43df6057e92321ff096998d87dfbd5	refs/tags/v0.0.0-runsheet-test.6^{}
$ gh release view v0.0.0-runsheet-test.<1..5> --repo runsheet/mesh-cli
release not found   (each of the five)
$ gh release list --repo runsheet/mesh-cli
v0.0.0-runsheet-test.6	Pre-release	v0.0.0-runsheet-test.6	2026-09-26T15:06:03Z
v0.0.0-selfupdate-test.1	Pre-release	v0.0.0-selfupdate-test.1	2026-09-25T13:33:49Z
```

## Release

```
$ gh release view v0.0.0-runsheet-test.6 --repo runsheet/mesh-cli
title:	v0.0.0-runsheet-test.6
tag:	v0.0.0-runsheet-test.6
draft:	false
prerelease:	true
immutable:	false
author:	github-actions[bot]
created:	2026-09-26T14:43:31Z
published:	2026-09-26T15:06:03Z
url:	https://github.com/runsheet/mesh-cli/releases/tag/v0.0.0-runsheet-test.6
asset:	mesh-darwin-arm64
asset:	mesh-linux-amd64
asset:	mesh-linux-arm64
asset:	SHA256SUMS
asset:	SHA256SUMS.sigstore.json
--
Release v0.0.0-runsheet-test.6 from `184011c99d43df6057e92321ff096998d87dfbd5`. SHA256SUMS is signed keyless with Sigstore by .github/workflows/release-tag.yml; see the README's Release section to verify.

$ gh release download v0.0.0-runsheet-test.6 --repo runsheet/mesh-cli -D /tmp/rel6
$ cat /tmp/rel6/SHA256SUMS
564194299a86e1ea8e5a7b36f24c60c9c51d9b9549290d4ba5afeb4ce7bcac98  mesh-darwin-arm64
737ae40d34cb6705147f94d1150bb29fb9138c6817f03478eb64123c4ceb77c5  mesh-linux-amd64
1378ab1952e9f0327da7d40eacd7beaad369f4efd718179e3c57b4f5230ec756  mesh-linux-arm64
$ (cd /tmp/rel6 && sha256sum -c SHA256SUMS)
mesh-darwin-arm64: OK
mesh-linux-amd64: OK
mesh-linux-arm64: OK

# the workflow's own check, from run 36249480345's log
Sign and publish v0.0.0-runsheet-test.6	Sign SHA256SUMS (keyless)	tlog entry created with index: 2968738457
Sign and publish v0.0.0-runsheet-test.6	Verify the signature against the release identity	Verified OK
```

## Node image

`ghcr.io/runsheet/mesh-node:v0.0.0-runsheet-test.6` was pushed as
`sha256:e4fcd4f5b8a2f03da71dc50006837047c6b200ab0f1a2b6ecf1bbdddd9901e41`,
then signed keyless and verified by the image job. The `cosign verify`
step that #206 added ran green for the first time on a tag cut from
main. Excerpt from run **36249480345**:

```
Node image v0.0.0-runsheet-test.6	Build and push …	#23 pushing manifest for ghcr.io/runsheet/mesh-node:v0.0.0-runsheet-test.6@sha256:e4fcd4f5b8a2f03da71dc50006837047c6b200ab0f1a2b6ecf1bbdddd9901e41 4.8s done
Node image v0.0.0-runsheet-test.6	Sign the image (keyless)	tlog entry created with index: 2968737209
Node image v0.0.0-runsheet-test.6	Verify the image's signature against the release identity	Run cosign verify "ghcr.io/runsheet/mesh-node@${DIGEST}" \
    --certificate-identity "https://github.com/runsheet/mesh-cli/.github/workflows/release-tag.yml@${GITHUB_REF}" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
  DIGEST: sha256:e4fcd4f5b8a2f03da71dc50006837047c6b200ab0f1a2b6ecf1bbdddd9901e41
Verification for ghcr.io/runsheet/mesh-node@sha256:e4fcd4f5b8a2f03da71dc50006837047c6b200ab0f1a2b6ecf1bbdddd9901e41 --
The following checks were performed on each of these signatures:
  - The cosign claims were validated
  - Existence of the claims in the transparency log was verified offline
  - The code-signing certificate was verified using trusted certificate authority certificates
[{"critical":{"identity":{"docker-reference":"ghcr.io/runsheet/mesh-node"},"image":{"docker-manifest-digest":"sha256:e4fcd4f5b8a2f03da71dc50006837047c6b200ab0f1a2b6ecf1bbdddd9901e41"}, …
  "Subject":"https://github.com/runsheet/mesh-cli/.github/workflows/release-tag.yml@refs/tags/v0.0.0-runsheet-test.6",
  "githubWorkflowRef":"refs/tags/v0.0.0-runsheet-test.6", …}]
```

**Visibility: anonymous pulls are refused.** Nodes that pull
anonymously cannot fetch `ghcr.io/runsheet/mesh-node`:

```
$ skopeo inspect --no-creds docker://ghcr.io/runsheet/mesh-node:v0.0.0-runsheet-test.6
level=fatal msg="Error parsing image name \"docker://ghcr.io/runsheet/mesh-node:v0.0.0-runsheet-test.6\": unable to retrieve auth token: invalid username/password: unauthorized"
exit=1
$ curl -s "https://ghcr.io/token?scope=repository:runsheet/mesh-node:pull&service=ghcr.io"
{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}
```

A sandboxed node's `EnsureImage` treats a failed pull as an
`updateError`: no drain and no swap. So until the operator acts, a
sandboxed node that is not logged in refuses every update. The
`projectmesh-io` package was the same. The operator must do one of
these:

- make the package public (or grant its readers access) under
  github.com/orgs/runsheet/packages/container/mesh-node/settings; or
- log each sandboxed node in to ghcr (`podman login ghcr.io`) with a
  classic token holding `read:packages` from an account that can read
  the `runsheet` org's packages; or
- set node.yaml `sandbox.image: build` on the node to build the image
  locally (ADR 002, node image amendment).

This session changed no org or package settings.

## Verification

The binary is built from this branch, which is `origin/main`
(`184011c`) plus this record. The verifier (`internal/selfupdate`,
`internal/commands/nodeupdate.go`) is main's. The repository is
private, so the plain fetch from GitHub gets a 404, as it did in PR
#168. The checks read the downloaded release through `--from-dir`, the
path #168 added for this:

```
$ go build -o /tmp/mesh8 ./cmd/mesh
$ go test ./internal/selfupdate/...
ok  	projectmesh.io/mesh-cli/internal/selfupdate

# the new tag, under the runsheet identity
$ mesh node update --verify-only --from-dir /tmp/rel6 v0.0.0-runsheet-test.6
Fetching mesh-linux-amd64 of v0.0.0-runsheet-test.6 and verifying it…
Verified: mesh-linux-amd64 sha256 737ae40d34cb6705147f94d1150bb29fb9138c6817f03478eb64123c4ceb77c5 is in SHA256SUMS, signed by release-tag.yml on refs/tags/v0.0.0-runsheet-test.6.
Verify only: written to /tmp/mesh-update-2023425556/mesh-linux-amd64; nothing swapped.
exit=0

# the pre-transfer reference tag, still accepted under the projectmesh-io identity
$ gh release download v0.0.0-selfupdate-test.1 --repo runsheet/mesh-cli -D /tmp/rel-old
$ mesh node update --verify-only --from-dir /tmp/rel-old v0.0.0-selfupdate-test.1
Fetching mesh-linux-amd64 of v0.0.0-selfupdate-test.1 and verifying it…
Verified: mesh-linux-amd64 sha256 70878e3301cdbb0e10a0bfcdb26cf1f4614db8010c17860f0209d995a59d12c8 is in SHA256SUMS, signed by release-tag.yml on refs/tags/v0.0.0-selfupdate-test.1.
Verify only: written to /tmp/mesh-update-2990202144/mesh-linux-amd64; nothing swapped.
exit=0

# refused: the same files named as another tag
$ mesh node update --verify-only --from-dir /tmp/rel6 v0.0.0-runsheet-test.7
Error: refused v0.0.0-runsheet-test.7: SHA256SUMS is not signed by release-tag.yml on v0.0.0-runsheet-test.7
exit=1
$ mesh node update --verify-only --from-dir /tmp/rel-old v0.0.0-runsheet-test.6
Error: refused v0.0.0-runsheet-test.6: SHA256SUMS is not signed by release-tag.yml on v0.0.0-runsheet-test.6
exit=1

# the plain fetch, while the repository is private
$ mesh node update --verify-only v0.0.0-runsheet-test.6
Error: refused v0.0.0-runsheet-test.6: release v0.0.0-runsheet-test.6 has no SHA256SUMS on GitHub (404: missing, or the repo is private)
exit=1
```

A node on main's binary accepts a release signed by the runsheet
identity. It still accepts the one pre-transfer tag under the old
identity, and it refuses a release named as a tag its signature does
not cover. Fetching over plain HTTPS keeps failing while the repository
is private. That is the gap #168 recorded; the transfer did not change
it.

# v0.2.187: the first signed release on the public mirror (goal signed-releases-on-runsheet-mesh, 2026-09-28)

Recorded 2026-09-28 for item 6 of goal `signed-releases-on-runsheet-mesh`,
the retry of item 4. Item 4 rightly refused to tag while #316 was open.
#315 (the mirror step) and #316 (the node downloads from runsheet/mesh)
are both on main now. This checks the contract in ADR 002's amendments
"the public mirror on runsheet/mesh" and "the node downloads from
runsheet/mesh".

**Outcome in one line:** the tag, the signed release on
runsheet/mesh-cli and the superseding of v0.1.0 are done. The mirror to
runsheet/mesh **failed**. The cause is a bug in
`scripts/mirror-release.sh`, not RELEASE_TOKEN (see Mirror step), so
the anonymous `--verify-only` from runsheet/mesh is still refused.

## Tag

Before the tag, `git ls-remote --tags origin` listed no `v0.2.187`, and
`gh release list --repo runsheet/mesh-cli` listed only `v0.1.0` and the
two test prereleases. The annotated tag `v0.2.187` was pushed at
`origin/main`'s head, the merge of #324, not from a branch:

```
$ git ls-remote --tags origin v0.2.187
b4cf92e2a8c5f8a611092b8e9b8dde79dddac506	refs/tags/v0.2.187
$ git rev-list -n1 v0.2.187
909202bb9a2081e220cacdaafb07349da5ca66a0
$ git merge-base --is-ancestor $(git rev-list -n1 v0.2.187) origin/main; echo exit=$?
exit=0
```

## Release

```
$ gh run list --repo runsheet/mesh-cli --workflow release-tag.yml -L 2
completed	failure	The node runs the repo's declared validation on the finished commit a…	Release (tag)	v0.2.187	push	36494313388	42m43s	2026-09-28T22:46:08Z
completed	success	A heartbeat's scaling.accounts is always a list, [] when the node nam…	Release (tag)	v0.1.0	push	36390918390	29m10s	2026-09-28T07:18:04Z

$ gh run view 36494313388 --repo runsheet/mesh-cli --json jobs -q '.jobs[]|"\(.name)\t\(.conclusion)"'
Build linux/arm64	success
Build darwin/arm64	success
Node image v0.2.187	success
Build linux/amd64	success
Sign and publish v0.2.187	failure

$ gh release view v0.2.187 --repo runsheet/mesh-cli
title:	v0.2.187
tag:	v0.2.187
draft:	false
prerelease:	false
immutable:	false
author:	github-actions[bot]
created:	2026-09-28T22:46:04Z
published:	2026-09-28T23:28:42Z
url:	https://github.com/runsheet/mesh-cli/releases/tag/v0.2.187
asset:	mesh-darwin-arm64
asset:	mesh-linux-amd64
asset:	mesh-linux-arm64
asset:	SHA256SUMS
asset:	SHA256SUMS.sigstore.json
--
Release v0.2.187 from `909202bb9a2081e220cacdaafb07349da5ca66a0`. SHA256SUMS is signed keyless with Sigstore by `.github/workflows/release-tag.yml`; see the README's Release section to verify.

### Commits

- Prompts, INDEX.md and the channel brief tell agents to use mesh_document/mesh_search on mesh-context; … (#258) (514de9f)
  … (the merged PR titles since v0.1.0, from scripts/release-notes.sh, #317) …
- The node runs the repo's declared validation on the finished commit and uploads the attestation the platform signs: … (#324) (909202b)

$ gh release download v0.2.187 --repo runsheet/mesh-cli -D /tmp/rel187
$ cat /tmp/rel187/SHA256SUMS
d1bb5c093fb9e0731a3452f9ca6da4515c1f0fc77d8f75a3983d36a547067477  mesh-darwin-arm64
bc00b2197b8e5b5712997b58c21f02ddf9965b65bb29c7abdb222a5936923ce7  mesh-linux-amd64
e592245640054568fcba9194cbf9a73c2159bb5441a75207e59f80e3b19c624b  mesh-linux-arm64
$ (cd /tmp/rel187 && sha256sum -c SHA256SUMS)
mesh-darwin-arm64: OK
mesh-linux-amd64: OK
mesh-linux-arm64: OK

# the workflow's own check, from run 36494313388's publish job
Sign SHA256SUMS (keyless)	tlog entry created with index: 2989849475
Sign SHA256SUMS (keyless)	Wrote bundle to file SHA256SUMS.sigstore.json
Verify the signature against the release identity	Verified OK
Create release on runsheet/mesh-cli	https://github.com/runsheet/mesh-cli/releases/tag/v0.2.187
```

The signed release on runsheet/mesh-cli is complete, with all five
assets. The run is `failure` only because of its last step, the mirror.

## Mirror step

The step 'Mirror the release to runsheet/mesh'
(`scripts/mirror-release.sh`) failed on its first command,
`gh release create`. It never reached the byte-for-byte SHA256SUMS
check, the cosign verify-blob or the README push. From run
**36494313388**'s publish-job log:

```
Sign and publish v0.2.187	Mirror the release to runsheet/mesh	##[group]Run scripts/mirror-release.sh "$TAG" "$SHA" release
  GH_TOKEN: ***
  GH_REPO: runsheet/mesh
  TAG: v0.2.187
  SHA: 909202bb9a2081e220cacdaafb07349da5ca66a0
Sign and publish v0.2.187	Mirror the release to runsheet/mesh	2026-09-28T23:28:44.3714852Z tag v0.2.187 exists locally but has not been pushed to runsheet/mesh, please push it before continuing or specify the `--target` flag to create a new tag
Sign and publish v0.2.187	Mirror the release to runsheet/mesh	2026-09-28T23:28:44.4038660Z ##[error]Process completed with exit code 1.
```

**This is not the RELEASE_TOKEN failure the brief foresaw.** The job's
checkout (full history, for the release notes) holds the tag `v0.2.187`.
runsheet/mesh does not have it, because the mirror carries none of
mesh-cli's history. When the tag exists locally but not on the target
repo, `gh release create` refuses unless `--target` names a ref, so it
asks GitHub to create the tag there. ADR 002's amendment intends this:
"GitHub creates it there on the mirror's default branch, as
`cli-release.yml` did". The script passes no `--target`. The tests for
#315 did not catch it because they ran without a local tag. The token
was never exercised for a write, so whether RELEASE_TOKEN (dated
2026-04-18) reaches the transferred repo is still unproven.

```
$ gh release view v0.2.187 --repo runsheet/mesh
release not found
$ gh api repos/runsheet/mesh --jq '.default_branch,.private'
main
false
```

**The operator's fix** (nothing was deleted or recreated by this item,
and nothing was pushed to runsheet/mesh):

1. Land a one-line fix to `scripts/mirror-release.sh`: pass
   `--target "$(gh api "repos/${GH_REPO}" --jq .default_branch)"` (that
   is, `main`) to `gh release create`. This is a code change, outside
   this item's docs-only diff.
2. Do not just `gh run rerun 36494313388`. The rerun checks out the tag,
   which carries the unfixed script, so it fails the same way. It also
   fails earlier, because `Create release on runsheet/mesh-cli` would
   find v0.2.187 already there. Either:
   - mirror v0.2.187 by hand with the fixed script. From a checkout of
     the fix, with `/tmp/rel187` holding the five assets downloaded
     above:
     `GH_TOKEN=<RELEASE_TOKEN> GH_REPO=runsheet/mesh NOTES="$(gh release view v0.2.187 --repo runsheet/mesh-cli --json body -q .body)" scripts/mirror-release.sh v0.2.187 909202bb9a2081e220cacdaafb07349da5ca66a0 /tmp/rel187`.
     The bytes are the ones release-tag.yml signed, and the script
     itself cosign-verifies them against the runsheet/mesh-cli identity
     before it is done. Or:
   - after the fix merges, cut `v0.2.188` from main and let the workflow
     mirror it. v0.2.187 then stays a mesh-cli-only release.
3. If the fixed step then fails on the token (a 403/404 on the
   create), replace RELEASE_TOKEN with a fine-grained token whose
   resource owner is the runsheet org, scoped to runsheet/mesh with
   Contents: read and write, or with the runsheet org App's installation
   token.

**Re-checked 2026-09-28 23:33Z (the item's second attempt, rebased on
`e09dbaf`):** nothing has changed. `origin/main`'s
`scripts/mirror-release.sh` still passes no `--target`, and no pull
request carries the fix. `gh release view v0.2.187 --repo runsheet/mesh`
answers `release not found`, and the SHA256SUMS URL answers `HTTP/2 404`.
The plain `--verify-only` below is still refused with exit 1. This item
cannot change that. Its diff is docs only, and its token cannot write
runsheet/mesh. Step 1 and one of the two routes in step 2 are what
remain.

**Re-checked 2026-09-28 23:38Z (the item's third attempt):** the fix now
exists as an open pull request,
[#336](https://github.com/runsheet/mesh-cli/pull/336) ("mirror-release:
create the mirror's release at main (--target)", head `ba0e737`, only
`scripts/mirror-release.sh`, mergeable), but it is not merged. Nothing
on runsheet/mesh has changed: `gh release view v0.2.187 --repo
runsheet/mesh` answers `release not found`, the SHA256SUMS URL answers
`HTTP/2 404`, and a binary built from this branch still refuses the
plain `--verify-only v0.2.187` with exit 1. What remains is the
operator's: merge #336, then take one of the two routes in step 2.

## Verification

The binary is built from this branch, which is `origin/main`
(`909202b`) plus this record. Its `internal/selfupdate` is main's:
`ReleaseBaseURL` is `https://github.com/runsheet/mesh/releases/download/`,
fetched anonymously, and the verifier checks against `IdentityPrefix`
for runsheet/mesh-cli.

```
$ go build -o /tmp/mesh187 ./cmd/mesh
$ go test ./internal/selfupdate/...
ok  	projectmesh.io/mesh-cli/internal/selfupdate	0.169s

# the plain fetch from the mirror (no --from-dir): refused, since the mirror step failed
$ mesh node update --verify-only v0.2.187
Fetching mesh-linux-amd64 of v0.2.187 and verifying it…
Error: refused v0.2.187: release v0.2.187 has no SHA256SUMS on runsheet/mesh (404: not published there, or not a signed release)
exit=1

$ curl -sI https://github.com/runsheet/mesh/releases/download/v0.2.187/SHA256SUMS | head -1
HTTP/2 404
# the same URL shape answers a redirect for a release that is there
$ curl -sI https://github.com/runsheet/mesh/releases/download/v0.2.186/mesh-linux-amd64 | grep -iE '^HTTP|^location'
HTTP/2 302
location: https://release-assets.githubusercontent.com/github-production-release-asset/1214072934/…
$ gh api repos/runsheet/mesh/releases/latest --jq .tag_name
v0.2.186

# the same release's files, from runsheet/mesh-cli: accepted under the mesh-cli identity
$ mesh node update --verify-only --from-dir /tmp/rel187 v0.2.187
Fetching mesh-linux-amd64 of v0.2.187 and verifying it…
Verified: mesh-linux-amd64 sha256 bc00b2197b8e5b5712997b58c21f02ddf9965b65bb29c7abdb222a5936923ce7 is in SHA256SUMS, signed by release-tag.yml on refs/tags/v0.2.187.
Verify only: written to /tmp/mesh-update-3207547979/mesh-linux-amd64; nothing swapped.
exit=0

# negative: the same files named as v0.2.188
$ mesh node update --verify-only --from-dir /tmp/rel187 v0.2.188
Fetching mesh-linux-amd64 of v0.2.188 and verifying it…
Error: refused v0.2.188: SHA256SUMS is not signed by release-tag.yml on v0.2.188
exit=1
```

The signed bytes verify. A release named as a tag its signature does
not cover is refused. The anonymous download from runsheet/mesh has
nothing to fetch until the mirror step publishes v0.2.187 there. Until
then, `mesh node update` with no version takes the mirror's latest,
`v0.2.186`, and refuses it for its missing SHA256SUMS (ADR 002, "the
node downloads from runsheet/mesh"). Once the mirror holds v0.2.187,
re-running the first command above is the check that it works.

## Superseded

v0.1.0 is the only `v0.1.x` release on runsheet/mesh-cli. It is now a
prerelease with a note, and its tag is untouched
(`2f79e5e…`, `refs/tags/v0.1.0^{}` → `9ed5f8b`):

```
$ gh release edit v0.1.0 --repo runsheet/mesh-cli --prerelease --notes '<the original notes>

Superseded: mesh-cli'"'"'s releases continue the public line at v0.2.187 on the public mirror runsheet/mesh (ADR 002, amendment "the public mirror on runsheet/mesh"); this tag is left as it was.'
$ gh release view v0.1.0 --repo runsheet/mesh-cli --json isPrerelease,body
{"body":"Release v0.1.0 from `9ed5f8b7f7c7f4cea34ae260068ef0331481230e`. SHA256SUMS is signed keyless with Sigstore by .github/workflows/release-tag.yml; see the README's Release section to verify.\n\nSuperseded: mesh-cli's releases continue the public line at v0.2.187 on the public mirror runsheet/mesh (ADR 002, amendment \"the public mirror on runsheet/mesh\"); this tag is left as it was.","isPrerelease":true}
$ gh api repos/runsheet/mesh-cli/releases/latest --jq .tag_name
v0.2.187
$ gh release list --repo runsheet/mesh-cli
v0.2.187	Latest	v0.2.187	2026-09-28T23:28:42Z
v0.1.0	Pre-release	v0.1.0	2026-09-28T07:47:09Z
v0.0.0-runsheet-test.6	Pre-release	v0.0.0-runsheet-test.6	2026-09-26T15:06:03Z
v0.0.0-selfupdate-test.1	Pre-release	v0.0.0-selfupdate-test.1	2026-09-25T13:33:49Z
```

## The mini

The live update is the operator's check, and this item did not do it.
It needs v0.2.187 on runsheet/mesh first (Mirror step above). Then the
operator sets `setNodeDesiredVersion v0.2.187` for the mini from the
console, the node fetches from runsheet/mesh with no by-hand copy,
verifies against the runsheet/mesh-cli identity, swaps and restarts
while adopting its sessions, and `~/.mesh/update.json` confirms on the
first heartbeat. None of that was done by this item.
