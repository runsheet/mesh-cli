---
summary: The first tag cut from runsheet/mesh-cli's main after the transfer (v0.0.0-runsheet-test.6) — its release, its signed node image under ghcr.io/runsheet and `mesh node update --verify-only` accepting it; and v0.2.187, the first release on the runsheet/mesh mirror, held until item 2 (#316, the node's mirror Source) is on main.
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

## v0.2.187: the first release on the mirror

Item 4 of goal `signed-releases-on-runsheet-mesh`. **Not cut yet:** the
item's precondition does not hold. The tag needs both item 1
(release-tag.yml's mirror step) and item 2 (the node's Source reading
runsheet/mesh) on `main`. Only item 1 is there. No tag was pushed, the
mini was not touched, and v0.1.0 was not edited. The five subsections
(Tag, Both releases, v0.1.0 superseded, The mirror's README, The mini
updates from runsheet/mesh) are recorded once item 2 lands.

### Precondition, 2026-09-28

```
$ git log origin/main --oneline -3
a1e3e03 gateway: RecordValidationAttestation uploads a build's validation attestation ... (#318)
9a284f7 Signed releases return to the public mesh repo ...: release-tag.yml mirrors each signed release to runsheet/mesh ... (#315)
213d54e Codex sessions reach the context tool: ... (#305)
$ git merge-base --is-ancestor 22170a7 origin/main || echo notonmain   # item 2's selfupdate commit
notonmain
$ gh pr view 316 -R runsheet/mesh-cli --json state,mergeable,mergeStateStatus
{"mergeStateStatus":"DIRTY","mergeable":"CONFLICTING","state":"OPEN"}
$ git merge-tree --write-tree origin/main origin/mesh/signed-releases-on-runsheet-mesh-2
CONFLICT (content): Merge conflict in README.md
CONFLICT (content): Merge conflict in docs/adr/adr-002-signed-tag-releases.md
$ gh release view v0.2.187 -R runsheet/mesh
release not found
$ gh release view v0.2.187 -R runsheet/mesh-cli
release not found
```

A v0.2.187 cut from this `main` would ship a node whose
`ReleaseBaseURL` and `LatestReleaseURL` still name the private
runsheet/mesh-cli. Its next self-update would get the 404 that v0.1.0
got, and the goal's live check (`setNodeDesiredVersion v0.2.187` with
no by-hand copy) could not pass on the version it is named for. #316
has to be rebased onto `main` and merged first.
