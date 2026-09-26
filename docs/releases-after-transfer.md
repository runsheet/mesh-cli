---
summary: The first tag released from runsheet/mesh-cli after the transfer (v0.0.0-runsheet-test.3) — its release, its node image under ghcr.io/runsheet and `mesh node update --verify-only` accepting it and the pre-transfer reference tag, with what is still open.
---

# Releases after the transfer

Recorded 2026-09-26 for item 6 of goal `mesh-cli-lives-in-runsheet`, on
the model of the `v0.0.0-selfupdate-test.1` verification (PR #168). ADR
002's transfer amendment is the contract this checks.

**What is still open** (each said again where it belongs below):

- The verified tag is **not cut from main**: `release-tag.yml` on main
  (`64d7f7e`) cannot publish, and the three fixes that make it work are
  on this item's branch, not main. The tag is the branch's `ce31462`.
  Once the branch merges, cut `v0.0.0-runsheet-test.<n>` from main and
  append its run here.
- **`cosign verify` of the image was not run from here**: the package is
  private and the session's item token cannot read it. The signature
  was checked through its Rekor entry instead. The command is below for
  someone with a token that can read the package.
- **The package stays private** as the `projectmesh-io` one was;
  nothing was changed on it (this session cannot).

## Tag

Four tags were pushed from runsheet/mesh-cli; `release-tag.yml` ran for
each.

| Tag | Commit | Run | Result |
| --- | --- | --- | --- |
| `v0.0.0-runsheet-test.1` | `d32b963` (main, #201) | 36243231991 | failure: the arm64 image's `nft --version` under qemu-user |
| `v0.0.0-runsheet-test.2` | `b2971cc` (branch) | 36244616825 | failure: `publish` downloaded the image's `.dockerbuild` record as an asset |
| `v0.0.0-runsheet-test.3` | `ce31462` (branch) | 36245606760 | **success** |
| `v0.0.0-runsheet-test.4` | `9bd81a9` (branch) | 36245651812 | failure: `cosign sign` of the pushed image, `UNAUTHORIZED` |

The fixes, in order, are this branch's commits:

- `e3c494a` fix(image): check nftables by its package
  (`dpkg-query -W nftables`); `nft --version` needs netlink, which the
  arm64 build under qemu-user lacks.
- `ce31462` fix(release): `publish` downloads only the `mesh-*`
  binaries, not the image's build record.
- `9bd81a9` ci(release): the image job runs `cosign verify` on the
  pushed image against the release identity.

`.4`'s failure came after its push had finished (13:54:59Z). The
`cosign sign` step could not get a pull token for the package it had
just pushed:

```
Error: signing [ghcr.io/runsheet/mesh-node@sha256:caa774aef893019f466b6ca2cb4b518abf448c8b3ea7fe2b99e11ad240979858]: accessing image: GET https://ghcr.io/token?scope=repository%3Arunsheet%2Fmesh-node%3Apull&service=ghcr.io: UNAUTHORIZED: authentication required
```

`.3`'s run signed with the same step 2 minutes earlier. The two ran
concurrently against a package that did not yet exist. That points at
the package's first creation, not the workflow, but this was not
proven. A re-run was refused to this session (`Resource not accessible
by integration`). The `cosign verify` step `9bd81a9` adds has therefore
not yet run green. The tag cut from main after the merge exercises it.

```
$ gh run list --repo runsheet/mesh-cli --workflow release-tag.yml -L 5
completed	failure	Release (tag)	Release (tag)	v0.0.0-runsheet-test.4	push	36245651812	21m10s	2026-09-26T13:35:03Z
completed	success	Release (tag)	Release (tag)	v0.0.0-runsheet-test.3	push	36245606760	22m10s	2026-09-26T13:34:13Z
completed	failure	Release (tag)	Release (tag)	v0.0.0-runsheet-test.2	push	36244616825	17m17s	2026-09-26T13:16:28Z
completed	failure	feat(release): node image under ghcr.io/runsheet/mesh-node; drop the …	Release (tag)	v0.0.0-runsheet-test.1	push	36243231991	17m0s	2026-09-26T12:51:11Z
completed	success	A node updates itself to the version the platform asks for: `mesh nod…	Release (tag)	v0.0.0-selfupdate-test.1	push	36136246020	53m50s	2026-09-25T12:40:05Z

$ gh run view 36245606760 --repo runsheet/mesh-cli --json jobs -q '.jobs[]|"\(.name)\t\(.conclusion)"'
Node image v0.0.0-runsheet-test.3	success
Build linux/amd64	success
Build linux/arm64	success
Build darwin/arm64	success
Sign and publish v0.0.0-runsheet-test.3	success
```

## Release

```
$ gh release view v0.0.0-runsheet-test.3 --repo runsheet/mesh-cli
title:	v0.0.0-runsheet-test.3
tag:	v0.0.0-runsheet-test.3
draft:	false
prerelease:	true
immutable:	false
author:	github-actions[bot]
created:	2026-09-26T13:34:10Z
published:	2026-09-26T13:56:14Z
url:	https://github.com/runsheet/mesh-cli/releases/tag/v0.0.0-runsheet-test.3
asset:	mesh-darwin-arm64
asset:	mesh-linux-amd64
asset:	mesh-linux-arm64
asset:	SHA256SUMS
asset:	SHA256SUMS.sigstore.json
--
Release v0.0.0-runsheet-test.3 from `ce31462e534ea60fd4f6cb1fd7497a688d7a1422`. SHA256SUMS is signed keyless with Sigstore by .github/workflows/release-tag.yml; see the README's Release section to verify.

$ gh release download v0.0.0-runsheet-test.3 --repo runsheet/mesh-cli -D /tmp/rel3
$ cat /tmp/rel3/SHA256SUMS
4394632b9eb122eac65349f846ff6a35aa180b41fcd7fa2216d10609840c69b1  mesh-darwin-arm64
c33c49593214f2b252b7dbfa804b77deae70dd8ec5c35476ee1e3d6202380166  mesh-linux-amd64
ac92e67f648282740280070699c694713c1cd19d4c13e3ee612e5397f410d98d  mesh-linux-arm64
$ (cd /tmp/rel3 && sha256sum -c SHA256SUMS)
mesh-darwin-arm64: OK
mesh-linux-amd64: OK
mesh-linux-arm64: OK

# the bundle's certificate names the runsheet identity
$ openssl x509 -in <bundle cert> -noout -ext subjectAltName
X509v3 Subject Alternative Name: critical
    URI:https://github.com/runsheet/mesh-cli/.github/workflows/release-tag.yml@refs/tags/v0.0.0-runsheet-test.3

# the workflow's own check, from the run's log
Sign and publish v0.0.0-runsheet-test.3	Sign SHA256SUMS (keyless)	tlog entry created with index: 2968581673
Sign and publish v0.0.0-runsheet-test.3	Verify the signature against the release identity	Verified OK
```

## Node image

`ghcr.io/runsheet/mesh-node:v0.0.0-runsheet-test.3` was pushed as
manifest list `sha256:d9bf371f1153bda8edc724f095448cee69ad099e06177bba809b921dc4dce525`
(linux/amd64 and linux/arm64) and signed keyless:

```
Node image v0.0.0-runsheet-test.3	Build and push …	#23 pushing manifest for ghcr.io/runsheet/mesh-node:v0.0.0-runsheet-test.3@sha256:d9bf371f1153bda8edc724f095448cee69ad099e06177bba809b921dc4dce525 4.3s done
Node image v0.0.0-runsheet-test.3	Sign the image (keyless)	  DIGEST: sha256:d9bf371f1153bda8edc724f095448cee69ad099e06177bba809b921dc4dce525
Node image v0.0.0-runsheet-test.3	Sign the image (keyless)	tlog entry created with index: 2968575867
Node image v0.0.0-runsheet-test.3	Sign the image (keyless)	Pushing signature to: ghcr.io/runsheet/mesh-node
```

**Visibility: private.** An anonymous pull token is refused, and so is
the session's GitHub App installation token:

```
$ curl -s "https://ghcr.io/token?scope=repository:runsheet/mesh-node:pull&service=ghcr.io"
{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}

$ cosign verify ghcr.io/runsheet/mesh-node:v0.0.0-runsheet-test.3 \
    --certificate-identity https://github.com/runsheet/mesh-cli/.github/workflows/release-tag.yml@refs/tags/v0.0.0-runsheet-test.3 \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
# anonymous:
Error: GET https://ghcr.io/token?scope=repository%3Arunsheet%2Fmesh-node%3Apull&service=ghcr.io: UNAUTHORIZED: authentication required
# after `cosign login ghcr.io -u x-access-token` with the item's installation token:
Error: GET https://ghcr.io/v2/runsheet/mesh-node/manifests/v0.0.0-runsheet-test.3: DENIED: requested access to the resource is denied
```

That matches the `projectmesh-io` package: private while the repository
is, and read by a node after `podman login ghcr.io` with a token that
can read it, or skipped with `sandbox.image: build` (ADR 002, node image
amendment). Nothing about access was changed here; this session cannot
administer org packages. For the nodes to pull, the operator must do one
of these:

- log each sandboxed node in with a classic token holding
  `read:packages` from an account with access to the `runsheet` org
  (`podman login ghcr.io`), as was done for `projectmesh-io`; or
- make the package public, or grant access to its readers, under
  github.com/orgs/runsheet/packages/container/mesh-node/settings.

The signature was checked without the registry, from its Rekor entry
(log index 2968575867, the one the run printed):

```
$ curl -s "https://rekor.sigstore.dev/api/v1/log/entries?logIndex=2968575867"
uuid 108e9186e8c5677ab5744ffcbce5271b13a489dbd27ab21b86b7f320892fee37a10c68f40ec41302
logIndex 2968575867 integratedTime 1790430814
kind hashedrekord 0.0.1
# its certificate:
X509v3 Subject Alternative Name: critical
    URI:https://github.com/runsheet/mesh-cli/.github/workflows/release-tag.yml@refs/tags/v0.0.0-runsheet-test.3
1.3.6.1.4.1.57264.1.1  (issuer):      https://token.actions.githubusercontent.com
1.3.6.1.4.1.57264.1.12 (source repo): https://github.com/runsheet/mesh-cli
1.3.6.1.4.1.57264.1.13 (source sha):  ce31462e534ea60fd4f6cb1fd7497a688d7a1422
1.3.6.1.4.1.57264.1.14 (source ref):  refs/tags/v0.0.0-runsheet-test.3
1.3.6.1.4.1.57264.1.20 (event):       push
```

The entry proves a signature by the runsheet release identity for that
tag. It does not prove which digest was signed, because the signed
payload lives in the registry. `cosign verify` above, run with a token
that can read the package, is the check that remains.

## Verification

The binary is built from this branch at `9bd81a9`. Its verifier
(`internal/selfupdate`, `internal/commands/nodeupdate.go`) is main's
unchanged (`git diff origin/main HEAD -- internal/selfupdate
internal/commands` is empty).

The repository is private, so the plain fetch from GitHub gets a 404
for both tags, as it did in PR #168. The check reads the release's own
files through `--from-dir`, the path #168 added for this:

```
$ go build -o /tmp/mesh ./cmd/mesh

$ mesh node update --verify-only v0.0.0-runsheet-test.3
Fetching mesh-linux-amd64 of v0.0.0-runsheet-test.3 and verifying it…
Error: refused v0.0.0-runsheet-test.3: release v0.0.0-runsheet-test.3 has no SHA256SUMS on GitHub (404: missing, or the repo is private)
exit 1

$ mesh node update --verify-only --from-dir /tmp/rel3 v0.0.0-runsheet-test.3
Fetching mesh-linux-amd64 of v0.0.0-runsheet-test.3 and verifying it…
Verified: mesh-linux-amd64 sha256 c33c49593214f2b252b7dbfa804b77deae70dd8ec5c35476ee1e3d6202380166 is in SHA256SUMS, signed by release-tag.yml on refs/tags/v0.0.0-runsheet-test.3.
Verify only: written to /tmp/mesh-update-201563640/mesh-linux-amd64; nothing swapped.
exit 0

# the pre-transfer reference tag, still accepted under the projectmesh-io identity
$ gh release download v0.0.0-selfupdate-test.1 --repo runsheet/mesh-cli -D /tmp/rel-old
$ mesh node update --verify-only --from-dir /tmp/rel-old v0.0.0-selfupdate-test.1
Fetching mesh-linux-amd64 of v0.0.0-selfupdate-test.1 and verifying it…
Verified: mesh-linux-amd64 sha256 70878e3301cdbb0e10a0bfcdb26cf1f4614db8010c17860f0209d995a59d12c8 is in SHA256SUMS, signed by release-tag.yml on refs/tags/v0.0.0-selfupdate-test.1.
Verify only: written to /tmp/mesh-update-3455998745/mesh-linux-amd64; nothing swapped.
exit 0

# refusals: the same files named as another tag, and an edited SHA256SUMS
$ mesh node update --verify-only --from-dir /tmp/rel3 v0.0.0-runsheet-test.4
Error: refused v0.0.0-runsheet-test.4: SHA256SUMS is not signed by release-tag.yml on v0.0.0-runsheet-test.4
exit 1
$ mesh node update --verify-only --from-dir /tmp/rel3-edited v0.0.0-runsheet-test.3
Error: refused v0.0.0-runsheet-test.3: SHA256SUMS signature does not verify
exit 1
```

A node on this binary accepts a release signed by the runsheet identity.
It also still accepts the one pre-transfer tag under the old identity.
Fetching over plain HTTPS keeps failing while the repository is private.
That is the same gap #168 recorded, and the transfer did not change it.
