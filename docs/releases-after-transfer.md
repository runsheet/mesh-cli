---
summary: The first tag cut from runsheet/mesh-cli's main after the transfer (v0.0.0-runsheet-test.6) and its verification; then v0.2.188, the first signed release on the public mirror runsheet/mesh (run 36499785802, attempt 3, after attempt 1's HTTP 403 from the stored mirror token), which the mini runs since 2026-09-29 00:52Z, sessions adopted and the update confirmed — and v0.2.187, cut and signed on the source runsheet/mesh-cli only (its mirror step failed without --target, fixed by #336); a 2026-09-29 addendum confirms desiredVersion is v0.2.188 (item 17's own evaluation already read it so; this item corrected nothing), why update.json's v0.1.0 refusal is stale, the live README's lag until v0.2.189, and v0.2.189 blocked on RELEASE_TAG_TOKEN (run 36507379094); a 2026-09-30 addendum records the mirror's token exchange through Rigger for a scoped credential, the binding .runsheet/bindings/mesh-cli-release.yaml, RELEASE_TOKEN as only the logged fallback until the runsheet half answers and then deleted, and the idempotent publish a re-run after a mirror failure relies on; a 2026-10-02 reading with scripts/mirror-token-reading.sh over the last five release-tag.yml runs finds every one still on the RELEASE_TOKEN fallback, the gateway refusing with "unauthenticated: the bearer is a JWT with no role (unknown key id)" while the runsheet goal rigger-binds-a-workflow-to-a-scoped-github-token is queued.
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

Recorded 2026-09-29 for items 10 and 14 of goal
`signed-releases-on-runsheet-mesh` (ADR 002, amendment "the public
mirror on runsheet/mesh"). `v0.2.188` is the first signed release on
`runsheet/mesh`. Run `36499785802`'s first attempt was refused by the
mirror with HTTP 403 (`RELEASE_TOKEN`). The operator replaced the token
and re-ran it; attempt 3 published the release on both repositories at
2026-09-29 00:50Z. The mini has run it since 00:51:59Z. `v0.2.187` was
cut and signed on `runsheet/mesh-cli` only.

### Tag

```
$ git ls-remote --tags origin v0.2.187 v0.2.188 'v0.2.187^{}' 'v0.2.188^{}'
b4cf92e2a8c5f8a611092b8e9b8dde79dddac506	refs/tags/v0.2.187
909202bb9a2081e220cacdaafb07349da5ca66a0	refs/tags/v0.2.187^{}
1d587a0ef52bfdcaf2ffa13bb26c4de82d4807f7	refs/tags/v0.2.188
5a8c29ea1f534f69fa64dbcaf5f7be7497d9ffb1	refs/tags/v0.2.188^{}

$ git ls-remote --tags origin 'v0.2.*'
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

`v0.2.188` is an annotated tag on `5a8c29e`, the merge of #336
(`--target main` for the mirror's release). It was pushed at
2026-09-28 23:46:50Z. No tag is newer.

### The run

Run `36499785802` (`release-tag.yml` on `v0.2.188`) took three
attempts.

**Attempt 1** (2026-09-29 00:03Z) failed at the last step, the mirror.
Every earlier step passed, including the release on
`runsheet/mesh-cli`:

```
$ gh run view 36499785802 --repo runsheet/mesh-cli --attempt 1 --log-failed | tail -2
Sign and publish v0.2.188	Mirror the release to runsheet/mesh	2026-09-29T00:03:31.8438936Z HTTP 403: Resource not accessible by personal access token (https://api.github.com/repos/runsheet/mesh/releases)
Sign and publish v0.2.188	Mirror the release to runsheet/mesh	2026-09-29T00:03:31.8601701Z ##[error]Process completed with exit code 1.
```

The error, verbatim:

```
HTTP 403: Resource not accessible by personal access token (https://api.github.com/repos/runsheet/mesh/releases)
```

This was the first line `scripts/mirror-release.sh` runs, `gh release
create "$tag" --target main …`, refused at `POST
/repos/runsheet/mesh/releases`. "Resource not accessible by personal
access token" is GitHub's answer to a fine-grained token without
permission on the repository it calls. `RELEASE_TOKEN` was the secret
the old `cli-release.yml` used while the mirror was
`projectmesh-io/mesh`. A fine-grained token's resource owner is fixed
when it is created, so it did not follow the repository to `runsheet`.
The token the mirror needs is a fine-grained PAT **owned by
`runsheet`**, with repository access **only `runsheet/mesh`** and
**Contents: Read and write**, stored as `RELEASE_TOKEN` on
`runsheet/mesh-cli`. The operator replaced it. No script changed.

**Attempt 2** (00:48Z) failed one step earlier, because the source
release from attempt 1 still existed:

```
$ gh run view 36499785802 --repo runsheet/mesh-cli --attempt 2 --log-failed | tail -2
Sign and publish v0.2.188	Create release on runsheet/mesh-cli	2026-09-29T00:48:14.7351910Z a release with the same tag name already exists: v0.2.188
Sign and publish v0.2.188	Create release on runsheet/mesh-cli	2026-09-29T00:48:14.7406779Z ##[error]Process completed with exit code 1.
```

**Attempt 3** (00:50Z) succeeded. The source release was created again
at 00:50:45Z (its `published` date below), with the tag left as it was:

```
$ gh run view 36499785802 --repo runsheet/mesh-cli --json attempt,conclusion
{"attempt":3,"conclusion":"success"}

$ gh run view 36499785802 --repo runsheet/mesh-cli
✓ v0.2.188 Release (tag) · 36499785802

JOBS
✓ Sign and publish v0.2.188 in 53s (ID 109204346201)
✓ Node image v0.2.188 in 9m58s (ID 109204346366)
✓ Build linux/amd64 in 29s (ID 109204346668)
✓ Build linux/arm64 in 18s (ID 109204371223)
✓ Build darwin/arm64 in 19s (ID 109204384361)
```

The mirror step's key lines from attempt 3's log:

```
$ gh run view 36499785802 --repo runsheet/mesh-cli --attempt 3 --log \
    | grep 'Mirror the release to runsheet/mesh' | cut -f3 \
    | grep -E 'mirror-release|GH_REPO|TAG:|releases/tag|Verified' | sed 's/\x1b\[[0-9;]*m//g'
2026-09-29T00:50:45.6479529Z ##[group]Run scripts/mirror-release.sh "$TAG" "$SHA" release
2026-09-29T00:50:45.6479912Z ^[[36;1mscripts/mirror-release.sh "$TAG" "$SHA" release^[[0m
- mirror-release: create the mirror's release at main (--target), the tag being ours alone (#336) (5a8c29e)
2026-09-29T00:50:45.6491100Z   GH_REPO: runsheet/mesh
2026-09-29T00:50:45.6491280Z   TAG: v0.2.188
2026-09-29T00:50:55.6686660Z https://github.com/runsheet/mesh/releases/tag/v0.2.188
2026-09-29T00:50:57.5565100Z Verified OK
```

- `https://github.com/runsheet/mesh/releases/tag/v0.2.188` is `gh
  release create`'s output: the mirror's release, with its five assets.
- The `cmp` of the source's `SHA256SUMS` against the one downloaded
  from the mirror prints nothing when the files are equal. Under `set
  -e` the script would have stopped there otherwise.
- `Verified OK` is `cosign verify-blob` on the mirror's bundle against
  the `runsheet/mesh-cli` identity.
- The README PUT's output goes to `/dev/null`. Its commit on the
  mirror, two seconds later, is the evidence:

```
$ gh api repos/runsheet/mesh/commits -q '.[0]|.sha[0:7]+" "+.commit.committer.date+" "+.commit.message'
4d8a6cc 2026-09-29T00:50:58Z mirror: update README from runsheet/mesh-cli@5a8c29ea1f534f69fa64dbcaf5f7be7497d9ffb1
```

### Both releases

```
$ gh release view v0.2.188 --repo runsheet/mesh-cli
title:	v0.2.188
tag:	v0.2.188
draft:	false
prerelease:	false
immutable:	false
author:	github-actions[bot]
created:	2026-09-28T23:46:50Z
published:	2026-09-29T00:50:45Z
url:	https://github.com/runsheet/mesh-cli/releases/tag/v0.2.188
asset:	mesh-darwin-arm64
asset:	mesh-linux-amd64
asset:	mesh-linux-arm64
asset:	SHA256SUMS
asset:	SHA256SUMS.sigstore.json
…
$ gh release view v0.2.188 --repo runsheet/mesh
title:	v0.2.188
tag:	v0.2.188
draft:	false
prerelease:	false
immutable:	false
author:	chriskolenko
created:	2026-04-23T07:41:05Z
published:	2026-09-29T00:50:55Z
url:	https://github.com/runsheet/mesh/releases/tag/v0.2.188
asset:	mesh-darwin-arm64
asset:	mesh-linux-amd64
asset:	mesh-linux-arm64
asset:	SHA256SUMS
asset:	SHA256SUMS.sigstore.json
…
$ gh release view v0.2.188 --repo runsheet/mesh --json assets -q '.assets[].name'
mesh-darwin-arm64
mesh-linux-amd64
mesh-linux-arm64
SHA256SUMS
SHA256SUMS.sigstore.json
$ gh api repos/runsheet/mesh/releases/latest -q .tag_name
v0.2.188
```

The mirror's release is authored by the `RELEASE_TOKEN`'s owner. Its
`created` date is that of the mirror's `main` head, where `--target
main` put its tag.

The notes are the same text. The source's body has one more trailing
newline, because the source release reads `--notes-file NOTES.md`
while the mirror's is `--notes "$NOTES"`:

```
$ for r in runsheet/mesh-cli runsheet/mesh; do gh release view v0.2.188 --repo $r --json body -q '.body|length'; done
1136
1135
$ for r in runsheet/mesh-cli runsheet/mesh; do gh release view v0.2.188 --repo $r --json body -q '.body|rtrimstr("\n")' | sha256sum; done
595b2bf65d224c6ddf13d6efa76d8bfeadabc944753b05a3e6ce1d393694ce12  -
595b2bf65d224c6ddf13d6efa76d8bfeadabc944753b05a3e6ce1d393694ce12  -
```

The signed files are the same bytes, and the mirror's verify:

```
$ gh release download v0.2.188 --repo runsheet/mesh-cli -D /tmp/src188
$ gh release download v0.2.188 --repo runsheet/mesh -D /tmp/mir188
$ cmp /tmp/src188/SHA256SUMS /tmp/mir188/SHA256SUMS && echo SHA256SUMS equal
SHA256SUMS equal
$ cd /tmp/mir188 && cat SHA256SUMS
5c55ec97df7058730b19ba5e5939b9a67b7a114e18f09c56aa1835717852bd19  mesh-darwin-arm64
d66057ea8d5ef005fc9add35af003e937180d87612225cbb0e4a477f2a2418f4  mesh-linux-amd64
8b057a4a77ca9d6c734a758dfd798fbe63f6e4df1811137f1f5674c4167e4012  mesh-linux-arm64
$ cosign verify-blob --bundle SHA256SUMS.sigstore.json \
    --certificate-identity https://github.com/runsheet/mesh-cli/.github/workflows/release-tag.yml@refs/tags/v0.2.188 \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com SHA256SUMS
Verified OK
$ sha256sum -c SHA256SUMS
mesh-darwin-arm64: OK
mesh-linux-amd64: OK
mesh-linux-arm64: OK

$ curl -sI https://github.com/runsheet/mesh/releases/download/v0.2.188/SHA256SUMS | head -1
HTTP/2 302
```

(cosign v2.5.2.)

### The updater fetches from the mirror

Built from this branch. With no `--from-dir` the updater fetches
anonymously from `runsheet/mesh`, as #316 made it, and verifies against
the `runsheet/mesh-cli` identity. With no version it takes the mirror's
latest. The same bytes named as another tag are refused:

```
$ go build -o /tmp/mesh ./cmd/mesh

$ /tmp/mesh node update --verify-only v0.2.188
Fetching mesh-linux-amd64 of v0.2.188 and verifying it…
Verified: mesh-linux-amd64 sha256 d66057ea8d5ef005fc9add35af003e937180d87612225cbb0e4a477f2a2418f4 is in SHA256SUMS, signed by release-tag.yml on refs/tags/v0.2.188.
Verify only: written to /tmp/mesh-update-1578607997/mesh-linux-amd64; nothing swapped.
exit 0

$ /tmp/mesh node update --verify-only
The latest release is v0.2.188.
Fetching mesh-linux-amd64 of v0.2.188 and verifying it…
Verified: mesh-linux-amd64 sha256 d66057ea8d5ef005fc9add35af003e937180d87612225cbb0e4a477f2a2418f4 is in SHA256SUMS, signed by release-tag.yml on refs/tags/v0.2.188.
Verify only: written to /tmp/mesh-update-1242862115/mesh-linux-amd64; nothing swapped.
exit 0

$ /tmp/mesh node update --verify-only --from-dir /tmp/mir188 v0.2.189
Fetching mesh-linux-amd64 of v0.2.189 and verifying it…
Error: refused v0.2.189: SHA256SUMS is not signed by release-tag.yml on v0.2.189
exit 1
```

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
token, which is why the 403 first showed up on v0.2.188. Item 10 added
one paragraph to its release notes on `runsheet/mesh-cli`. The tag and
the release are left as they were:

```
$ gh release view v0.2.187 --repo runsheet/mesh-cli --json body -q .body | grep -o '^Never mirrored: [^)]*)\.' | sed 's/^/v0.2.187: /'
v0.2.187: Never mirrored: the mirror step of run 36494313388 failed before it reached runsheet/mesh (gh release create without --target, fixed in #336).
```

That paragraph's second sentence was written before attempt 3. It still
calls v0.2.188 unmirrored and v0.2.189 the retry (see "What is owed").

### v0.1.0 superseded

The note now names v0.2.188 as the first release on the mirror. The
existing text is kept, `isPrerelease` stays true, and the tag is
untouched:

```
$ gh release view v0.1.0 --repo runsheet/mesh-cli --json body -q .body > v0.1.0-notes.md
$ sed -i 's|continue the public line at v0.2.187 on the public mirror runsheet/mesh|continue the public line at v0.2.188 on the public mirror runsheet/mesh (v0.2.187 was cut and signed on runsheet/mesh-cli only; its mirror step failed)|' v0.1.0-notes.md
$ gh release edit v0.1.0 --repo runsheet/mesh-cli --notes-file v0.1.0-notes.md
$ gh release view v0.1.0 --repo runsheet/mesh-cli --json isPrerelease,body
{"body":"Release v0.1.0 from `9ed5f8b7f7c7f4cea34ae260068ef0331481230e`. SHA256SUMS is signed keyless with Sigstore by .github/workflows/release-tag.yml; see the README's Release section to verify.\n\nSuperseded: mesh-cli's releases continue the public line at v0.2.188 on the public mirror runsheet/mesh (v0.2.187 was cut and signed on runsheet/mesh-cli only; its mirror step failed) (ADR 002, amendment \"the public mirror on runsheet/mesh\"); this tag is left as it was.\n","isPrerelease":true}
$ git ls-remote --tags origin v0.1.0
2f79e5e9b3ddd02d8f729d1129c53c0b0f1c1c32	refs/tags/v0.1.0
```

### The mirror's README

Attempt 3's README PUT made the live README byte-identical to the
`dist/mirror/README.md` that `v0.2.188` carries:

```
$ gh api repos/runsheet/mesh/contents/README.md --jq .content | base64 -d > /tmp/live-readme.md
$ git show v0.2.188:dist/mirror/README.md > /tmp/tag-readme.md
$ cmp /tmp/live-readme.md /tmp/tag-readme.md && echo identical
identical
$ wc -c /tmp/live-readme.md
2070 /tmp/live-readme.md
$ grep -n 'v0.2.187' /tmp/live-readme.md
6:v0.2.187 every release here is a signed tag release, identical to the
```

It still says "From v0.2.187", because that is what `main` held at
`5a8c29e`. This branch changes `dist/mirror/README.md` to "From
v0.2.188", saying that v0.2.187 exists on `runsheet/mesh-cli` only. The
next release's mirror step pushes that text. Nobody edits the mirror's
README by hand.

### The mini updates from runsheet/mesh

The mini runs v0.2.188. It is byte-identical to the mirror's
`mesh-linux-amd64`:

```
$ ~/bin/mesh version
mesh v0.2.188
$ sha256sum ~/bin/mesh ~/bin/mesh.prev
d66057ea8d5ef005fc9add35af003e937180d87612225cbb0e4a477f2a2418f4  /home/chris/bin/mesh
6ccdb3e1073b097e9be41be3ac1b4e7d3a8907c82af7453eb4116533df8c3304  /home/chris/bin/mesh.prev
$ ~/bin/mesh.prev version
mesh 4d8d435
```

node.log, 2026/09/29 00:40–00:53:

```
$ grep -n -E '^mesh node: 2026/09/29 00:(4[0-9]|5[0-3])' ~/.mesh/node.log | grep -E 'v0.2.188|v0.1.0|adopt|confirm'
5803127:mesh node: 2026/09/29 00:40:21 self-update: 4d8d435 → v0.1.0: fetching
5803128:mesh node: 2026/09/29 00:40:22 self-update to v0.1.0 refused: release v0.1.0 has no SHA256SUMS on GitHub (404: missing, or the repo is private)
5803262:mesh node: 2026/09/29 00:51:59 mesh node v0.2.188, pid 1397397, config /home/chris/.mesh/node.yaml
5803263:mesh node: 2026/09/29 00:51:59 confirming update 4d8d435 → v0.2.188: start 1 of 3, rolls back on the third
5803267:mesh node: 2026/09/29 00:52:06 adopted a plan session of 82c9d02f-3f3a-5298-8c6f-17de4b8fa0f6 (pid 1395781, running)
5803268:mesh node: 2026/09/29 00:52:06 adopted a plan session of 6c5f51b0-fd7b-50fa-b703-a927ea3a7c03 (pid 1395870, running)
5803269:mesh node: 2026/09/29 00:52:06 adopt: 2 session(s) of the process before this one taken up
5803270:mesh node: 2026/09/29 00:52:08 goal the-node-serves-its-owners-orgs: adopted its running planner session (pid 1395870)
5803271:mesh node: 2026/09/29 00:52:08 goal a-node-has-an-owner: adopted its running planner session (pid 1395781)
5803274:mesh node: 2026/09/29 00:52:19 self-update: v0.2.188 is up; update confirmed
5803275:mesh node: 2026/09/29 00:52:20 self-update: v0.2.188 → v0.1.0: fetching
5803276:mesh node: 2026/09/29 00:52:20 self-update to v0.1.0 refused: release v0.1.0 has no SHA256SUMS on runsheet/mesh (404: not published there, or not a signed release)
```

**How it was triggered.** It was neither the loop acting on
desiredVersion nor `mesh node update v0.2.188`. It was the operator
running `mesh node upgrade` over ssh. The evidence:

- The loop never fetched v0.2.188. Between 00:40 and 00:51:59 its only
  `self-update:` lines are for v0.1.0, the platform's desiredVersion
  for the mini, refused by the 4d8d435 binary.
- There was no drain, which `mesh node update` does before its swap.
  The old process's last lines at 00:51:59 are `context canceled` on
  every platform call and `stopped; running items were left for a
  retry`. That is the restart path.
- `systemd --user` restarted the service at the second the new process
  started. That is the `systemctl --user restart` that `mesh node
  upgrade` runs after its swap. An ssh login from the operator's
  machine opened in the same second, and one a few seconds earlier
  left `~/bin/mesh.new.sums`, whose content is v0.2.188's
  `SHA256SUMS`:

```
$ journalctl --user --since '2026-09-29 00:51' --until '2026-09-29 00:52:30' --no-pager | grep mesh-node.service
Sep 29 00:51:59 mini systemd[1153]: Stopping io.projectmesh.mesh-node.service - Mesh node: runs work items of goals on this machine...
Sep 29 00:51:59 mini systemd[1153]: Stopped io.projectmesh.mesh-node.service - Mesh node: runs work items of goals on this machine.
Sep 29 00:51:59 mini systemd[1153]: io.projectmesh.mesh-node.service: Consumed 9min 37.561s CPU time.
Sep 29 00:51:59 mini systemd[1153]: Started io.projectmesh.mesh-node.service - Mesh node: runs work items of goals on this machine.
$ journalctl --since '2026-09-29 00:51' --until '2026-09-29 00:52:30' --no-pager | grep 'Accepted publickey'
Sep 29 00:51:39 mini sshd[1396899]: Accepted publickey for chris from 192.168.1.102 port 62749 ssh2: ED25519 SHA256:CBu3iG/tutvQxdCUzQVYbak3BiIxxUmgYNX/Rfpql8Q
Sep 29 00:51:59 mini sshd[1397320]: Accepted publickey for chris from 192.168.1.102 port 62766 ssh2: ED25519 SHA256:CBu3iG/tutvQxdCUzQVYbak3BiIxxUmgYNX/Rfpql8Q
$ ls -l --time-style=full-iso ~/bin/mesh ~/bin/mesh.new.sums
-rwxr-xr-x 1 chris chris 22986914 2026-09-29 00:51:59.772160675 +0000 /home/chris/bin/mesh
-rw-rw-r-- 1 chris chris      250 2026-09-29 00:51:45.180804669 +0000 /home/chris/bin/mesh.new.sums
$ cmp ~/bin/mesh.new.sums /tmp/mir188/SHA256SUMS && echo same
same
```

`~/.bash_history` was last written on 2026-09-24, so it has no record
of the command. There are two ways it could have been run. `mesh node
upgrade --version v0.2.188` fetches and verifies from `runsheet/mesh`
through `internal/selfupdate`. `--binary ~/bin/mesh.new` swaps in a
file the operator fetched from the mirror. The mini does not say which
of the two it was. No `~/bin/mesh.new` is left. Either way, the bytes
are the mirror's signed `mesh-linux-amd64`. The update was recorded as
pending (`confirming update 4d8d435 → v0.2.188: start 1 of 3`) and
confirmed by the first heartbeat 20 s later.

**Sessions survived.** The two planner sessions started at 00:50:27 and
00:50:33 (pids 1395781 and 1395870) ran through the restart in their
own units. The new process adopted both at 00:52:06 (the `adopted`
lines above).

**update.json now:**

```
$ cat ~/.mesh/update.json
{
  "from": "v0.2.188",
  "to": "v0.1.0",
  "attempts": 0,
  "lastError": "release v0.1.0 has no SHA256SUMS on runsheet/mesh (404: not published there, or not a signed release)"
}
```

It records the refusal of v0.1.0 that followed at 00:52:20, when the
loop read the platform's desiredVersion. v0.2.188 did not fail:
`Confirm` cleared its pending state at 00:52:19, and v0.1.0 was never
published on the mirror.

### What is owed

1. **desiredVersion.** The platform's desiredVersion for the mini is
   still `v0.1.0`. The loop refuses it after every heartbeat and records
   the `updateError` above, then waits an hour on that version before
   trying again. The operator sets it to `v0.2.188` from the console's
   Update menu (`setNodeDesiredVersion`). A node session does not set
   it.
2. **The console's release list.** `meshCliReleases` has to read
   `runsheet/mesh`, so that the Update menu lists exactly what a node
   can fetch. That work is owed on `runsheet/runsheet`.
3. **v0.2.189 is simply the next tag.** ADR 002's rule is the highest
   `v0.2.x` on `runsheet/mesh` plus one, which is now `v0.2.189`. Its
   mirror step pushes this branch's `dist/mirror/README.md` and the
   binding `.runsheet/bindings/mesh-cli-release.yaml`, when their
   content differs (see "The mirror's token is exchanged, not stored"
   below).
4. **v0.2.187's notes.** The second sentence of v0.2.187's "Never
   mirrored" paragraph is stale. It still calls v0.2.188 not yet
   mirrored and v0.2.189 the retry. Editing those notes is the
   operator's call. This item leaves the v0.2.187 release as it was.

## The channel on 2026-09-29

Recorded 2026-09-29 for item 21 of goal `signed-releases-on-runsheet-mesh`.
It is an addendum, checked live against the platform and GitHub; it
corrects nothing above and rewrites no earlier section.

### desiredVersion, confirmed

"What is owed" item 1 above recorded desiredVersion still at `v0.1.0`.
It no longer is: item 17's own evaluation — attempt 1, 2026-09-29
01:44:30Z, on the platform now — already read it as `v0.2.188`, in the
course of reviewing item 17's diff:

> Review: FAIL. The supplied diff changes only the five permitted
> files, and the platform reports Mini online at v0.2.188 with desired
> version v0.2.188. The indexed main ADR still contains the earlier,
> unmirrored account, so I could not confirm an `already_done` outcome;
> PR #385's status was unavailable.

So there was nothing for this item to correct there — the operator had
already moved desiredVersion to `v0.2.188` by the time item 17 ran. A
fresh live read today, with the `mesh_health_nodes` tool (project
`runsheet/mesh`), agrees:

```
$ mesh_health_nodes(project="runsheet/mesh")
{"at":"2026-09-29T02:35:34Z","latest_version":"v0.2.188","main_head":"132766a02b6cab1166222d5a3c0fda7f01994afd","nodes":[{"behind":true,"capacity":6,"commits_behind_main":9,"desired_version":"v0.2.188","draining":false,"heartbeat_age_seconds":5,"id":"313f7f4d-8a16-594f-b9b2-8af7faab3328","last_seen_at":"2026-09-29T02:35:28Z","name":"Mini","oldest_unrun_merged_at":"2026-09-29T00:23:02Z","org":"runsheet","plan_seen_at":"2026-09-29T02:35:28Z","plan_status":"allowed_warning","plan_windows":[{"resets_at":"2026-09-29T03:10:00Z","used_pct":6,"window":"five_hour"},{"resets_at":"2026-10-03T03:00:00Z","used_pct":75,"window":"seven_day"}],"running":6,"sessions":[{"age_seconds":1104,"cost_cents":0,"harness":"claude-code","kind":"build","model":"opus","project":"runsheet","session_id":"fd6190d8-4a39-4e1f-a0e2-cb13ecf056fa","started_at":"2026-09-29T02:17:09.931269848Z","subject":"#1 Schema and policy: a Node has an owner (person | enterprise), one row per node, TransferNode and ShareNodeWithOrg, with the ADR (a-node-has-an-owner)","tools":[{"name":"Mesh context","slug":"mesh-context"},{"name":"Context7","slug":"context7"}]},{"age_seconds":776,"cost_cents":0,"harness":"claude-code","kind":"build","model":"sonnet","project":"runsheet","session_id":"a28525bc-4613-4f51-8977-a2f6b2eb53fe","started_at":"2026-09-29T02:22:37.339536893Z","subject":"#2 mesh_health_service reports process lag: the age of the oldest unhandled event per runner (a-new-org-opens-when-its-tenant-is-ready)","tools":[{"name":"Mesh context","slug":"mesh-context"},{"name":"Context7","slug":"context7"}]},{"age_seconds":210,"cost_cents":330,"harness":"claude-code","kind":"evaluate","model":"opus","project":"runsheet","session_id":"8a506129-e21b-4be4-a7dd-1a09d295915e","started_at":"2026-09-29T02:32:03.682418725Z","subject":"#2 Platform: a Tool may belong to an enterprise and attaches to any member org's project (credentials-follow-the-enterprise)","tools":[{"name":"Mesh context","slug":"mesh-context"},{"name":"Playwright","slug":"playwright"}]},{"age_seconds":207,"cost_cents":294,"harness":"claude-code","kind":"build","model":"sonnet","project":"runsheet","session_id":"8fe2b7fa-c356-4227-ab73-edb6f2c92be2","started_at":"2026-09-29T02:32:06.665942578Z","subject":"#23 Goal doc and design pass: specs/goal-integrator-lives-within-the-rate-limit.md records what shipped and how the day is measured; console-trd's Health and Scorecard carry the budget (integrator-lives-within-the-rate-limit)","tools":[{"name":"Mesh context","slug":"mesh-context"},{"name":"Context7","slug":"context7"}]},{"age_seconds":61,"cost_cents":33,"harness":"claude-code","kind":"evaluate","model":"opus","project":"loom","session_id":"95c0bd00-9f78-4815-b9c8-af03255181c5","started_at":"2026-09-29T02:34:32.782110723Z","subject":"#3 e2e: a nested type's optional list is nullable through the live gateway, an omitted or null list is accepted and arrives as a nil slice (nested-optional-lists-are-nullable)","tools":[{"name":"Mesh context","slug":"mesh-context"},{"name":"Playwright","slug":"playwright"}]},{"age_seconds":45,"cost_cents":78,"harness":"claude-code","kind":"build","model":"sonnet","project":"mesh","started_at":"2026-09-29T02:34:48.438060764Z","subject":"#21 docs/releases-after-transfer.md: a dated addendum of the mirror channel's state on 2026-09-29 — desiredVersion, update.json, the mirror README's lag, and the auto-tag blocked on RELEASE_TAG_TOKEN (signed-releases-on-runsheet-mesh)","tools":[{"name":"Mesh context","slug":"mesh-context"},{"name":"Context7","slug":"context7"}]},{"age_seconds":29,"cost_cents":295,"harness":"claude-code","kind":"evaluate","model":"opus","project":"runsheet","session_id":"b180a5ec-0127-4e0f-8ee8-40bf905a7296","started_at":"2026-09-29T02:35:04.365334386Z","subject":"#2 mesh_health_stuck and mesh_health_deploys answer within an overall deadline: what finished is returned, and each row or section that could not finish names its error (health-tools-always-answer)","tools":[{"name":"Mesh context","slug":"mesh-context"},{"name":"Context7","slug":"context7"}]},{"age_seconds":12,"cost_cents":0,"harness":"claude-code","kind":"build","model":"opus","project":"runsheet","started_at":"2026-09-29T02:35:21.447953997Z","subject":"#3 Platform: the metering API — POST /billing/v1/usage with idempotency, UsageReport and the ProductMeter fold, authenticated by the product's Latchkey client (one-account-one-bill-across-products)","tools":[]}],"slug":"mini","status":"online","version":"v0.2.188"}]}
```

The `sessions` list is every other live build, evaluate and plan session
the mini is running right now, on other goals and other projects — this
item's own build among them (`#21 docs/releases-after-transfer.md
…`, started 02:34:48Z); none of it bears on desiredVersion, which is
the one field this check is for: `"desired_version":"v0.2.188"`,
alongside `"version":"v0.2.188"`, exactly as the tool returned them.
`~/.mesh/node.log`'s latest self-update lines — both the `self-update:`
progress lines and the `self-update to <version> refused:` lines, so the
pattern takes either — agree there is nothing left to refuse:

```
$ grep -nE '^mesh node: [0-9/ :]+ self-update( to |:)' ~/.mesh/node.log | tail -4
5803128:mesh node: 2026/09/29 00:40:22 self-update to v0.1.0 refused: release v0.1.0 has no SHA256SUMS on GitHub (404: missing, or the repo is private)
5803274:mesh node: 2026/09/29 00:52:19 self-update: v0.2.188 is up; update confirmed
5803275:mesh node: 2026/09/29 00:52:20 self-update: v0.2.188 → v0.1.0: fetching
5803276:mesh node: 2026/09/29 00:52:20 self-update to v0.1.0 refused: release v0.1.0 has no SHA256SUMS on runsheet/mesh (404: not published there, or not a signed release)
```

The last two lines are one attempt: at 00:52:20 the `fetching` line
records the loop starting to fetch the desiredVersion of that moment,
`v0.1.0`, and the `refused` line after it is the refusal itself — no
`SHA256SUMS` for `v0.1.0` on runsheet/mesh. That refusal is the one
"What is owed" above recorded, before the operator moved desiredVersion.
Nothing has logged since, because with desiredVersion now `v0.2.188`,
equal to the running version, the loop has nothing to fetch
(`GetDesiredVersion`: "empty, the running one or `dev` does nothing").
`v0.2.188` is correct: confirmed twice now, by
item 17's evaluation and by this item's own live read, not corrected by
either.

### update.json still holds the old refusal

```
$ cat ~/.mesh/update.json
{
  "from": "v0.2.188",
  "to": "v0.1.0",
  "attempts": 0,
  "lastError": "release v0.1.0 has no SHA256SUMS on runsheet/mesh (404: not published there, or not a signed release)"
}
```

This is the same file the September 29 section above already quotes,
unchanged, because the loop writes `update.json` only on an attempt
(`selfupdate`, adr-002) and it has not attempted anything since
00:52:20. It is stale now that desiredVersion reads `v0.2.188`: nobody
has asked the mini for `v0.1.0` since, and no start failed or was left
unconfirmed against `v0.2.188`. The console's `updateError` for the
mini, if it still shows this `lastError`, is quoting the *last*
refusal on record, not a live condition — a stale refusal is expected
to sit in `update.json` between attempts, not a sign the mini is stuck.
This item changed nothing under `~/.mesh` and made no update attempt of
its own.

### The live README still says v0.2.187

```
$ gh api repos/runsheet/mesh/readme -q .content | base64 -d | head -12
# mesh

The public release mirror of the private source, `runsheet/mesh-cli`.
Releases v0.2.1 through v0.2.186 were unsigned builds of every push to
`main`, published here by the old `cli-release.yml` channel. From
v0.2.187 every release here is a signed tag release, identical to the
one `runsheet/mesh-cli`'s `release-tag.yml` publishes to itself
(docs/adr/adr-002-signed-tag-releases.md in `runsheet/mesh-cli`, the
amendment "the public mirror on runsheet/mesh").

## Install
```

The line reading `From v0.2.187 every release here is a signed tag
release` is what `5a8c29e`'s mirror step pushed, and it is what the
"The mirror's README" section above already found. `main`'s own
`dist/mirror/README.md` reads "From v0.2.188" today (this branch
touches neither file). Nobody edits the live mirror README by hand —
the mirror step of the *next* release that reaches
`scripts/mirror-release.sh`'s README `PUT` pushes `main`'s copy over
it, so the live README corrects itself to "From v0.2.188" only once
that release runs, i.e. at v0.2.189.

### v0.2.189 has not been cut

```
$ gh run list --repo runsheet/mesh-cli --workflow release-on-merge.yml -L 3
completed	failure	The built-in context tool is called mesh-context everywhere an agent …	Release on merge	main	push	36507379094	2m8s	2026-09-29T01:19:49Z
```

```
$ gh run view 36507379094 --repo runsheet/mesh-cli --log-failed
Tag the merge	UNKNOWN STEP	2026-09-29T01:21:51.1994450Z ^[[36;1mif [ -z "$RELEASE_TAG_TOKEN" ]; then^[[0m
Tag the merge	UNKNOWN STEP	2026-09-29T01:21:51.2000245Z ^[[36;1m  echo "::error::RELEASE_TAG_TOKEN is not set. Create a fine-grained token with Contents: read and write on ${GITHUB_REPOSITORY} and add it as the repository secret RELEASE_TAG_TOKEN. GITHUB_TOKEN cannot be used: a tag it pushes starts no workflow, so release-tag.yml would never run (docs/adr/adr-002-signed-tag-releases.md)."^[[0m
Tag the merge	UNKNOWN STEP	2026-09-29T01:21:51.2006045Z ^[[36;1m  exit 1^[[0m
Tag the merge	UNKNOWN STEP	2026-09-29T01:21:51.2423608Z ##[error]RELEASE_TAG_TOKEN is not set. Create a fine-grained token with Contents: read and write on runsheet/mesh-cli and add it as the repository secret RELEASE_TAG_TOKEN. GITHUB_TOKEN cannot be used: a tag it pushes starts no workflow, so release-tag.yml would never run (docs/adr/adr-002-signed-tag-releases.md).
##[error]Process completed with exit code 1.
```

Run `36507379094` (`release-on-merge.yml` on the merge of #320, "The
built-in context tool is called mesh-context…", 2026-09-29 01:19:49Z)
failed in job `Tag the merge` at its own guard clause, before it
computed a version or pushed anything: `RELEASE_TAG_TOKEN is not set`.
That secret is `scripts/release-on-merge.sh`'s own — a fine-grained
token with `Contents: read and write` on `runsheet/mesh-cli`, because
`GITHUB_TOKEN`'s push starts no workflow. Adding it is the operator's
to do, and it belongs to goal `node-releases-on-merge`, not this one.
So `v0.2.189` — the next tag ADR 002 names, one past the mirror's
highest `v0.2.x` — is simply not cut yet; nothing pushed a tag after
`v0.2.188`.

### The mirror's token is exchanged, not stored

Recorded 2026-09-30 for item 3 of goal
`release-tag-mirrors-with-a-rigger-binding`. v0.2.188's attempt 1 (run
`36499785802`, 2026-09-29 00:03Z) failed at the mirror with `HTTP 403:
Resource not accessible by personal access token`. The stored secret
then was a fine-grained token owned by `projectmesh-io`, created
2026-04-18. The mirror repository had moved to `runsheet`, and nobody
was told. Chris made a new token by hand (owner `runsheet`, repository
`runsheet/mesh` only, Contents: read and write) to unblock v0.2.188
(see "The run" above). That token expires and belongs to a person:
exactly the long-lived key Rigger exists to end.

What replaces it: the mirror step of `release-tag.yml` requests this
run's OIDC token with audience `runsheet-rigger` (the job's `id-token:
write`, already there for Sigstore) and exchanges it at the runsheet
gateway (`exchangeWorkflowToken`, `scripts/exchange-workflow-token.sh`)
for a contents:write token on `runsheet/mesh` alone, at most an hour
old. The platform checks the run against the binding the mirror
carries, `.runsheet/bindings/mesh-cli-release.yaml`, which is published
from `dist/mirror/` with each release whose content differs, as the
README is. The step logs which path it took: `mirror token: exchanged
through rigger, expires <time>`, or `mirror token: RELEASE_TOKEN
fallback (exchange failed: <reason>)` when it falls back. The
`RELEASE_TOKEN` fallback lasts only until the runsheet half (goal
`rigger-binds-a-workflow-to-a-scoped-github-token`) answers the
exchange; then the fallback secret is deleted from `runsheet/mesh-cli`,
and the fallback branch with it.

A re-run of the publish job after a mirror failure no longer needs a
release deleted by hand. `scripts/ensure-release.sh` keeps an existing
release of the tag whose assets are identical and fails on one that
differs; it deletes nothing. See
`docs/adr/adr-015-workflow-tokens-exchanged-through-rigger.md` and ADR
002's amendments "the public mirror on runsheet/mesh" and "the publish
job is idempotent".

#### The mirror token path, read

Recorded 2026-10-02 for item 6 of goal
`release-tag-mirrors-with-a-rigger-binding`. Item 4 failed three times
because nobody could answer "has a v* run mirrored on the exchanged
token yet?" without reading run logs by hand. `scripts/mirror-token-reading.sh`
(tested against fixture logs, `scripts/mirror-token-reading_test.sh`)
reads the "Mirror the release to runsheet/mesh" step's own `mirror
token: ` line of each of the last `count` completed `release-tag.yml`
runs and says which path it took. Run live against `runsheet/mesh-cli`
from this session:

```
$ scripts/mirror-token-reading.sh 5
v0.2.234	37035625041	2026-10-02T16:41:25Z	absent	failed at Install cosign
v0.2.233	37032523771	2026-10-02T16:14:02Z	fallback	refused: unauthenticated: the bearer is a JWT with no role (unknown key id)
v0.2.232	37031328695	2026-10-02T16:03:45Z	fallback	refused: unauthenticated: the bearer is a JWT with no role (unknown key id)
v0.2.231	37026904615	2026-10-02T15:25:44Z	fallback	refused: unauthenticated: the bearer is a JWT with no role (unknown key id)
v0.2.230	37023236264	2026-10-02T14:55:00Z	fallback	refused: unauthenticated: the bearer is a JWT with no role (unknown key id)
latest: fallback (v0.2.233, run 37032523771)
```

Every run that reached the step still falls back to `RELEASE_TOKEN`:
the gateway refuses the exchange with `unauthenticated: the bearer is a
JWT with no role (unknown key id)`, naming no binding and no claim —
the runsheet goal `rigger-binds-a-workflow-to-a-scoped-github-token`
was still queued when this reading was taken, so `exchangeWorkflowToken`
has nothing on the other end to issue against yet. `v0.2.234` never
reached the step at all: its `Sign and publish` job failed earlier, at
`Install cosign` (`cosign-installer` could not validate the bootstrap
download), unrelated to the token exchange. This reading changes
nothing here — the fallback stays until a run reads `exchanged` — it is
only the measure item 7 and item 8 read before they remove it.

Item 12 (2026-10-02) extends the script with a final `secret:` line
reading whether the `RELEASE_TOKEN` secret itself still exists on
`runsheet/mesh-cli` (`gh secret list`), so the goal's acceptance — "no
RELEASE_TOKEN secret exists on mesh-cli" — is readable too; today's run
says `secret: unknown (failed to get secrets: HTTP 403: Resource not
accessible by integration (https://api.github.com/repos/runsheet/mesh-cli/actions/secrets?per_page=100))`,
because this node's own credential has no admin scope on the repo, not
because the secret is gone — the secret still exists until item 10 or
a later check with admin scope confirms its deletion.

### Both repositories today

```
$ gh release list --repo runsheet/mesh -L 3
v0.2.188	Latest	v0.2.188	2026-09-29T00:50:55Z
v0.2.186		v0.2.186	2026-09-26T11:48:49Z
v0.2.185		v0.2.185	2026-09-26T11:40:50Z

$ gh release list --repo runsheet/mesh-cli -L 4
v0.2.188	Latest	v0.2.188	2026-09-29T00:50:45Z
v0.2.187		v0.2.187	2026-09-28T23:28:42Z
v0.1.0	Pre-release	v0.1.0	2026-09-28T07:47:09Z
v0.0.0-runsheet-test.6	Pre-release	v0.0.0-runsheet-test.6	2026-09-26T15:06:03Z

$ gh release view v0.1.0 --repo runsheet/mesh-cli --json isPrerelease -q .isPrerelease
true
```

`runsheet/mesh` has not moved since `v0.2.188` — no `v0.2.189` to
mirror, because none has been cut on the source. `runsheet/mesh-cli`
still lists `v0.2.187` above `v0.1.0`: it is the one release never
mirrored (see "v0.2.187, cut but never mirrored" above), unchanged by
this item. `v0.1.0` is still `isPrerelease: true`, as the "v0.1.0
superseded" section above left it.

### What stays owed

1. **`meshCliReleases` reading `runsheet/mesh`.** Still owed on
   `runsheet/runsheet`, so the console's Update menu lists exactly what
   a node can fetch, as "What is owed" above already said.
2. **`RELEASE_TAG_TOKEN`.** Not set on `runsheet/mesh-cli`. Until the
   operator adds it, `release-on-merge.yml`'s tag job fails at the
   guard above on every node-changing merge, and no `v0.2.189` — or any
   tag after it — is cut. Goal `node-releases-on-merge`, not this one.
3. **v0.2.187's stale release notes.** Unchanged; still the operator's
   call, as "What is owed" item 4 already recorded.
