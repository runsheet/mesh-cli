---
summary: The first tag cut from runsheet/mesh-cli's main after the transfer (v0.0.0-runsheet-test.6) and its verification; then v0.2.188, the first release cut for the runsheet/mesh mirror — signed on runsheet/mesh-cli, its mirror step refused by runsheet/mesh (HTTP 403, RELEASE_TOKEN), v0.2.189 the retry — and v0.2.187, cut and signed on the source only (its mirror step failed without --target, fixed by #336). The mini's update from runsheet/mesh (item 11) did not run: v0.2.188 is not on the mirror, so the mini stays on 4d8d435 with update.json to v0.1.0, waiting for v0.2.189.
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

## v0.2.188: the first release on the mirror

Recorded 2026-09-29 for item 10 of goal
`signed-releases-on-runsheet-mesh` (ADR 002, amendment "the public
mirror on runsheet/mesh"). `v0.2.188` is the first tag cut to put a
signed release on `runsheet/mesh`. It is signed and released on
`runsheet/mesh-cli`, but **it is not on the mirror yet**: the mirror
step's first API call was refused with HTTP 403 by the token
(`RELEASE_TOKEN`). The cause is outside this repository, so this
branch changes no script or workflow. `v0.2.189` is the retry, once the
token can write `runsheet/mesh` (below, "What is owed").

### Tag

```
$ git ls-remote --tags origin v0.2.187 v0.2.188 'v0.2.187^{}' 'v0.2.188^{}'
b4cf92e2a8c5f8a611092b8e9b8dde79dddac506	refs/tags/v0.2.187
909202bb9a2081e220cacdaafb07349da5ca66a0	refs/tags/v0.2.187^{}
1d587a0ef52bfdcaf2ffa13bb26c4de82d4807f7	refs/tags/v0.2.188
5a8c29ea1f534f69fa64dbcaf5f7be7497d9ffb1	refs/tags/v0.2.188^{}

$ git for-each-ref refs/tags/v0.2.187 refs/tags/v0.2.188 \
    --format='%(refname:short) %(objectname:short) %(*objectname:short) %(taggerdate:iso8601) %(taggername)'
v0.2.187 b4cf92e 909202b 2026-09-28 22:46:04 +0000 Chris Kolenko
v0.2.188 1d587a0 5a8c29e 2026-09-29 09:46:50 +1000 Chris Kolenko

$ git merge-base --is-ancestor 5a8c29e origin/main && echo ancestor
ancestor
```

`v0.2.188` is an annotated tag on `5a8c29e`, which is `origin/main`'s
head and the merge of #336 (`--target main` for the mirror's release).
It was pushed at 2026-09-28 23:46:50Z.

### The run

Run `36499785802` (`release-tag.yml` on `v0.2.188`) **concluded
failure**. Every job passed except the last step of the publish job,
the mirror:

```
$ gh run view 36499785802 --repo runsheet/mesh-cli --json status,conclusion,jobs \
    -q '{status,conclusion,jobs:[.jobs[]|{name,conclusion}]}'
{"conclusion":"failure","jobs":[{"conclusion":"success","name":"Node image v0.2.188"},{"conclusion":"success","name":"Build linux/arm64"},{"conclusion":"success","name":"Build darwin/arm64"},{"conclusion":"success","name":"Build linux/amd64"},{"conclusion":"failure","name":"Sign and publish v0.2.188"}],"status":"completed"}

$ gh run view 36499785802 --repo runsheet/mesh-cli
X v0.2.188 Release (tag) · 36499785802
JOBS
✓ Node image v0.2.188 in 9m58s (ID 109187634951)
✓ Build linux/arm64 in 18s (ID 109187635289)
✓ Build darwin/arm64 in 19s (ID 109187635346)
✓ Build linux/amd64 in 29s (ID 109187635351)
X Sign and publish v0.2.188 in 51s (ID 109191750944)
  ✓ Set up job
  ✓ Checkout code (full history, for the release notes)
  ✓ Download artifacts
  ✓ Check the three assets
  ✓ Write release notes
  ✓ Generate SHA256SUMS
  ✓ Install cosign
  ✓ Sign SHA256SUMS (keyless)
  ✓ Verify the signature against the release identity
  ✓ Compute the release notes
  ✓ Create release on runsheet/mesh-cli
  X Mirror the release to runsheet/mesh
  ✓ Post Checkout code (full history, for the release notes)
  ✓ Complete job

$ gh run view 36499785802 --repo runsheet/mesh-cli --log-failed | tail -2
Sign and publish v0.2.188	Mirror the release to runsheet/mesh	2026-09-29T00:03:31.8438936Z HTTP 403: Resource not accessible by personal access token (https://api.github.com/repos/runsheet/mesh/releases)
Sign and publish v0.2.188	Mirror the release to runsheet/mesh	2026-09-29T00:03:31.8601701Z ##[error]Process completed with exit code 1.
```

The error, verbatim:

```
HTTP 403: Resource not accessible by personal access token (https://api.github.com/repos/runsheet/mesh/releases)
```

This is the first line `scripts/mirror-release.sh` runs, `gh release
create "$tag" --target main …`. #336 moved it past the v0.2.187 failure,
and it reached the API: `POST /repos/runsheet/mesh/releases`. Neither
the `cmp`, the `cosign verify-blob` ("Verified OK") nor the README PUT
ran. Nothing was created on the mirror (see "Both releases").

The cause is the token, not this repository. "Resource not accessible
by personal access token" is GitHub's answer to a **fine-grained**
personal access token that lacks the permission on the repository it
calls. `RELEASE_TOKEN` is the secret the old `cli-release.yml` used to
publish to this repo while it was `projectmesh-io/mesh`. A fine-grained
token's resource owner is fixed when the token is created, so a token
owned by `projectmesh-io` does not follow the repository to `runsheet`.
The node session cannot read the secret's metadata to confirm this
(`gh secret list --repo runsheet/mesh-cli`: `HTTP 403: Resource not
accessible by integration`). What the token needs to create a release
and push the README:

- a fine-grained PAT with **resource owner `runsheet`**, repository
  access **only `runsheet/mesh`**, repository permission **Contents:
  Read and write** (Metadata: Read comes with it), approved by an org
  owner if the `runsheet` org requires approval for fine-grained
  tokens; or
- the runsheet org App's installation token, with `contents: write` on
  `runsheet/mesh`;

stored as `RELEASE_TOKEN` on `runsheet/mesh-cli`. No script change is
owed: `--target main` got past the local-tag check, and the request
the token was refused on is the right one.

### Both releases

```
$ gh release view v0.2.188 --repo runsheet/mesh-cli
title:	v0.2.188
tag:	v0.2.188
draft:	false
prerelease:	false
author:	github-actions[bot]
created:	2026-09-28T23:46:50Z
published:	2026-09-29T00:03:30Z
url:	https://github.com/runsheet/mesh-cli/releases/tag/v0.2.188
asset:	mesh-darwin-arm64
asset:	mesh-linux-amd64
asset:	mesh-linux-arm64
…
$ gh release view v0.2.188 --repo runsheet/mesh-cli --json assets -q '.assets[].name'
mesh-darwin-arm64
mesh-linux-amd64
mesh-linux-arm64
SHA256SUMS
SHA256SUMS.sigstore.json

$ gh release view v0.2.188 --repo runsheet/mesh
release not found
$ gh release download v0.2.188 --repo runsheet/mesh -D /tmp/mirror188 -p SHA256SUMS -p SHA256SUMS.sigstore.json
release not found
exit 1
$ gh api repos/runsheet/mesh/releases/latest -q '.tag_name+" "+.published_at+" "+([.assets[].name]|join(","))'
v0.2.186 2026-09-26T11:48:49Z mesh-darwin-amd64,mesh-darwin-arm64,mesh-linux-amd64,mesh-linux-arm64,mesh-windows-amd64.exe,SHA256SUMS

$ curl -sI https://github.com/runsheet/mesh/releases/download/v0.2.188/SHA256SUMS | head -1
HTTP/2 404
$ curl -sI https://github.com/runsheet/mesh/releases/download/v0.2.186/mesh-linux-amd64 | head -1
HTTP/2 302
```

The mirror has no `v0.2.188`, so there is nothing to `cmp` against
yet. The source's signed files verify under the `runsheet/mesh-cli`
identity. The next tag's mirror step must hand these bytes on
unchanged:

```
$ gh release download v0.2.188 --repo runsheet/mesh-cli -D /tmp/v0.2.188.3k3P
$ cat /tmp/v0.2.188.3k3P/SHA256SUMS
5c55ec97df7058730b19ba5e5939b9a67b7a114e18f09c56aa1835717852bd19  mesh-darwin-arm64
d66057ea8d5ef005fc9add35af003e937180d87612225cbb0e4a477f2a2418f4  mesh-linux-amd64
8b057a4a77ca9d6c734a758dfd798fbe63f6e4df1811137f1f5674c4167e4012  mesh-linux-arm64
$ cosign verify-blob --bundle SHA256SUMS.sigstore.json \
    --certificate-identity https://github.com/runsheet/mesh-cli/.github/workflows/release-tag.yml@refs/tags/v0.2.188 \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com SHA256SUMS
Verified OK
```

### The updater fetches from the mirror

Built from `main` (`5a8c29e`, identical to `v0.2.188`). With no
`--from-dir` it fetches anonymously from `runsheet/mesh`, as #316 made
it. It finds nothing there yet and says so in the words ADR 002's
amendment gives it. The same bytes read from the source verify, and the
same bytes named as another tag are refused:

```
$ go build -o /tmp/mesh ./cmd/mesh

$ /tmp/mesh node update --verify-only v0.2.188
Fetching mesh-linux-amd64 of v0.2.188 and verifying it…
Error: refused v0.2.188: release v0.2.188 has no SHA256SUMS on runsheet/mesh (404: not published there, or not a signed release)
exit 1

$ /tmp/mesh node update --verify-only
The latest release is v0.2.186.
Fetching mesh-linux-amd64 of v0.2.186 and verifying it…
Error: refused v0.2.186: release v0.2.186 has no SHA256SUMS.sigstore.json on runsheet/mesh (404: not published there, or not a signed release)
exit 1

$ /tmp/mesh node update --verify-only --from-dir /tmp/v0.2.188.3k3P v0.2.188
Fetching mesh-linux-amd64 of v0.2.188 and verifying it…
Verified: mesh-linux-amd64 sha256 d66057ea8d5ef005fc9add35af003e937180d87612225cbb0e4a477f2a2418f4 is in SHA256SUMS, signed by release-tag.yml on refs/tags/v0.2.188.
Verify only: written to /tmp/mesh-update-2900293090/mesh-linux-amd64; nothing swapped.
exit 0

$ /tmp/mesh node update --verify-only --from-dir /tmp/v0.2.188.3k3P v0.2.189
Fetching mesh-linux-amd64 of v0.2.189 and verifying it…
Error: refused v0.2.189: SHA256SUMS is not signed by release-tag.yml on v0.2.189
exit 1
```

The anonymous fetch refuses and the signed bytes verify. A node told
`v0.2.188` today would refuse it, record an `updateError` and wait an
hour. It would not take an unsigned build.

### v0.2.187, cut but never mirrored

The tag was cut at 2026-09-28 22:46:04Z on `909202b`. Its release on
`runsheet/mesh-cli` was published at 23:28:42Z. Run `36494313388`'s
mirror step failed in `gh` itself, before any request reached
`runsheet/mesh`:

```
$ gh run view 36494313388 --repo runsheet/mesh-cli --log-failed | tail -2
Sign and publish v0.2.187	Mirror the release to runsheet/mesh	2026-09-28T23:28:44.3714852Z tag v0.2.187 exists locally but has not been pushed to runsheet/mesh, please push it before continuing or specify the `--target` flag to create a new tag
Sign and publish v0.2.187	Mirror the release to runsheet/mesh	2026-09-28T23:28:44.4038660Z ##[error]Process completed with exit code 1.
```

#336 (`5a8c29e`) added `--target main` to `scripts/mirror-release.sh`'s
`gh release create`. The mirror has no commit of ours, so its tag is
created at its default branch's head. v0.2.187 never reached the
token, which is why the 403 above first showed up on v0.2.188. Its
release notes on `runsheet/mesh-cli` keep their text and gain one
paragraph. The tag and the release are left as they were:

```
$ gh release view v0.2.187 --repo runsheet/mesh-cli --json body -q .body > v0.2.187-notes.md
$ printf '\nNever mirrored: …\n' >> v0.2.187-notes.md
$ gh release edit v0.2.187 --repo runsheet/mesh-cli --notes-file v0.2.187-notes.md
$ gh release view v0.2.187 --repo runsheet/mesh-cli --json body -q .body | tail -1
Never mirrored: the mirror step of run 36494313388 failed before it reached runsheet/mesh (gh release create without --target, fixed in #336). v0.2.188 is the first release cut for the public mirror, but its mirror step (run 36499785802) was refused by runsheet/mesh with HTTP 403 (RELEASE_TOKEN cannot create releases there), so it is not yet mirrored; v0.2.189 is the retry once the token is fixed. This tag is left as it was.
```

### v0.1.0 superseded

Already done by an earlier item; not edited here:

```
$ gh release view v0.1.0 --repo runsheet/mesh-cli --json isPrerelease,body
{"body":"Release v0.1.0 from `9ed5f8b7f7c7f4cea34ae260068ef0331481230e`. SHA256SUMS is signed keyless with Sigstore by .github/workflows/release-tag.yml; see the README's Release section to verify.\n\nSuperseded: mesh-cli's releases continue the public line at v0.2.187 on the public mirror runsheet/mesh (ADR 002, amendment \"the public mirror on runsheet/mesh\"); this tag is left as it was.","isPrerelease":true}
```

### The mirror's README

The mirror step never reached its README PUT, so the live README is
still the one-line original. It is not the `dist/mirror/README.md` that
`v0.2.188` carries:

```
$ gh api repos/runsheet/mesh/contents/README.md --jq .content | base64 -d
# mesh
$ gh api repos/runsheet/mesh/commits -q '.[0]|.sha[0:7]+" "+.commit.committer.date+" "+.commit.message'
968f2dc 2026-04-23T07:41:05Z Add reference systemd unit for mesh node daemon

$ gh api repos/runsheet/mesh/contents/README.md --jq .content | base64 -d > /tmp/live-readme.md
$ git show v0.2.188:dist/mirror/README.md > /tmp/tag-readme.md
$ cmp /tmp/live-readme.md /tmp/tag-readme.md
cmp: EOF on /tmp/live-readme.md after byte 6, in line 1
$ wc -c /tmp/live-readme.md /tmp/tag-readme.md
   6 /tmp/live-readme.md
2070 /tmp/tag-readme.md
```

This branch updates `dist/mirror/README.md`: the signed line starts at
`v0.2.188`, and `v0.2.187` is on `runsheet/mesh-cli` only. The next
successful mirror step pushes it.

### What is owed

1. **The token.** Replace `RELEASE_TOKEN` on `runsheet/mesh-cli` with a
   token that can write `runsheet/mesh`: a fine-grained PAT owned by
   `runsheet`, scoped to `runsheet/mesh` alone, with Contents: Read and
   write, or the org App's installation token with `contents: write`
   there (see "The run"). This is an operator's by-hand step. No node
   session can read or set the secret.
2. **The retry is `v0.2.189`**, cut from `main` once the token is in
   place and this branch has merged, so that the mirror's README is
   the new one. Do not re-run `v0.2.188`'s publish job: it would fail
   at `gh release create` on `runsheet/mesh-cli`, where the release
   already exists. `v0.2.187` and `v0.2.188` stay on `runsheet/mesh-cli`
   as they are, signed and never mirrored. The ADR 002 numbering rule
   (max `v0.2.x` on `runsheet/mesh` plus one) would still answer
   `v0.2.187` while the mirror stops at `v0.2.186`. Cut `v0.2.189` by
   name.
3. `v0.2.189`'s run then repeats this record's checks against the
   mirror: five assets and the same notes on both releases, `cmp`,
   `cosign verify-blob`, the 302, `mesh node update --verify-only
   v0.2.189` with no `--from-dir`, and the live README byte-identical to
   the tag's `dist/mirror/README.md`.

### The mini updates from runsheet/mesh

Not done. Item 11 of goal signed-releases-on-runsheet-mesh stopped at
its precondition on 2026-09-29 00:30Z: `v0.2.188` is not on
`runsheet/mesh`. Its mirror step was refused (HTTP 403, above), and no
`v0.2.189` has been cut yet. The mini was not upgraded. Nothing was
built, swapped or restarted, and no `node upgrade` ran, because it
would fetch from a release that does not exist:

```
$ gh release view v0.2.188 --repo runsheet/mesh
release not found
$ gh api repos/runsheet/mesh/releases/tags/v0.2.188
{"message":"Not Found",…,"status":"404"}
$ gh release list --repo runsheet/mesh --limit 1
v0.2.186	Latest	v0.2.186	2026-09-26T11:48:49Z
$ gh release view v0.2.188 --repo runsheet/mesh-cli --json tagName,publishedAt
{"publishedAt":"2026-09-29T00:03:30Z","tagName":"v0.2.188"}
$ gh release view v0.2.189 --repo runsheet/mesh-cli
release not found
```

The mini's state as it was left (read only):

```
$ cat ~/.mesh/update.json
{
  "from": "4d8d435",
  "to": "v0.1.0",
  "attempts": 0,
  "lastError": "release v0.1.0 has no SHA256SUMS on GitHub (404: missing, or the repo is private)"
}
$ systemctl --user list-units 'mesh-session-*' --no-pager   # unit names only
mesh-session-build-497942f9-3369-483a-a7ed-11f24d65ad53-b70f2d73.scope
mesh-session-build-5e896c00-45a9-4a05-8874-54c044a05956-8d75626c.scope
mesh-session-build-a1df8261-8b75-42f4-b6f7-79b8d7cfb551-7e6463f3.scope
mesh-session-evaluate-7b69281a-1a43-428a-a18b-798badce5b66-1d74586e.scope
mesh-session-plan-c42ba4d3-b149-5430-ba7b-6232dbbf6c54-67f3e081.scope
```

Rechecked by attempt 2 on 2026-09-29 00:33Z: nothing had changed, so
it stopped again at the precondition and nothing ran:

```
$ gh release view v0.2.188 --repo runsheet/mesh
release not found
$ gh release list --repo runsheet/mesh --limit 1
v0.2.186	Latest	v0.2.186	2026-09-26T11:48:49Z
$ cat ~/.mesh/update.json   # unchanged: to v0.1.0, the 404 lastError, no confirmedAt
$ systemctl --user list-units 'mesh-session-*' --no-pager   # unit names only
mesh-session-build-6bf3a18e-07cc-4323-ab45-6f47a0fb449b-f2d6e5df.scope
mesh-session-build-9a1d4ff1-335c-40b6-adf1-2c0c2ba344c9-2a209cb9.scope
mesh-session-build-a1df8261-8b75-42f4-b6f7-79b8d7cfb551-ede7fc06.scope
mesh-session-plan-c42ba4d3-b149-5430-ba7b-6232dbbf6c54-67f3e081.scope
mesh_health_nodes (org runsheet), node mini:
  "version":"4d8d435","desired_version":"v0.1.0","behind":true,
  "commits_behind_main":13,"latest_version":"v0.2.188"
```

`latest_version` there is read from runsheet/mesh-cli, where v0.2.188
exists; the mirror, which the node fetches from, does not have it.

Rechecked by attempt 3 on 2026-09-29 00:36Z. Still nothing on the
mirror past v0.2.186, so it stopped at the precondition again and
nothing ran:

```
$ gh release list --repo runsheet/mesh --limit 1
v0.2.186	Latest	v0.2.186	2026-09-26T11:48:49Z
$ gh release view v0.2.187|v0.2.188|v0.2.189|v0.2.190 --repo runsheet/mesh
release not found   (each)
$ gh release list --repo runsheet/mesh-cli --limit 2
v0.2.188	Latest	v0.2.188	2026-09-29T00:03:30Z
v0.2.187		v0.2.187	2026-09-28T23:28:42Z
$ cat ~/.mesh/update.json   # unchanged: to v0.1.0, attempts 0, the 404 lastError, no confirmedAt
$ systemctl --user list-units 'mesh-session-*' --no-pager   # unit names only
mesh-session-build-35a08bb0-cca4-4df5-838a-85e8b4aaf2fe-38e0ded7.scope
mesh-session-build-497942f9-3369-483a-a7ed-11f24d65ad53-c6004944.scope
mesh-session-build-6d5107fa-f5f9-42e0-bd82-2b25729ad5db-33efac60.scope
mesh-session-build-a1df8261-8b75-42f4-b6f7-79b8d7cfb551-bec21da4.scope
mesh-session-build-c6ece57c-305f-4882-89da-24428b10b243-068eee4d.scope
mesh-session-build-ce6e1293-529b-40c6-97ba-41d3895afe5f-a6855d83.scope
mesh_health_nodes (org runsheet, 00:36:26Z), node mini:
  "version":"4d8d435","desired_version":"v0.1.0","behind":true,
  "commits_behind_main":13,"latest_version":"v0.2.188"
```

`grep -c 'v0.2.188' ~/.mesh/node.log` counts 189 at 00:36Z, but none
of them is an install. They are this item's title, logged on every
claim, and git fetches of the `v0.2.188` tag (`* [new tag] v0.2.188`).

The platform's `desiredVersion` for the mini is still `v0.1.0`. After
the upgrade, the operator sets it on the console to the version the
mini runs, or to the next tag. That setting is the goal's no-by-hand
check for later versions.

What unblocks it: the token and `v0.2.189` ("What is owed" above).
Then this subsection's steps run against `v0.2.189`, not `v0.2.188`,
because `v0.2.188` will never be on the mirror:

1. Capture `~/.mesh/update.json`, `systemctl --user status
   io.projectmesh.mesh-node.service`, the `mesh-session-*` units and
   the mini's version on `mesh_health_nodes`.
2. Run `go build -o /tmp/mesh-cli ./cmd/mesh` from main and
   `/tmp/mesh-cli node upgrade --version v0.2.189`.
3. After the confirm window, `update.json` should show `"to":
   "v0.2.189"` with a `confirmedAt`. node.log should show `confirming
   update 4d8d435 → v0.2.189` and the `adopt` lines, and
   `mesh_health_nodes` should report `v0.2.189`.
