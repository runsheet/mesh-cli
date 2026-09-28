# mesh

The public release mirror of the private source, `runsheet/mesh-cli`.
Releases v0.2.1 through v0.2.186 were unsigned builds of every push to
`main`, published here by the old `cli-release.yml` channel. From
v0.2.187 every release here is a signed tag release, identical to the
one `runsheet/mesh-cli`'s `release-tag.yml` publishes to itself
(docs/adr/adr-002-signed-tag-releases.md in `runsheet/mesh-cli`, the
amendment "the public mirror on runsheet/mesh").

## Install

Download `mesh-<os>-<arch>` from the
[latest release](https://github.com/runsheet/mesh/releases/latest),
make it executable and put it on `PATH`:

```sh
curl -LO https://github.com/runsheet/mesh/releases/latest/download/mesh-linux-amd64
chmod +x mesh-linux-amd64
sudo mv mesh-linux-amd64 /usr/local/bin/mesh
```

`dist/systemd` in this repo has the unit for running `mesh node start`
as a service.

## Verify a download

Every release's `SHA256SUMS` is signed keyless with Sigstore by the
workflow that built it — `runsheet/mesh-cli`'s, not this repo's, since
that is where it ran:

```sh
TAG=vX.Y.Z
gh release download "$TAG" -R runsheet/mesh \
  -p SHA256SUMS -p SHA256SUMS.sigstore.json -p mesh-linux-amd64
cosign verify-blob \
  --bundle SHA256SUMS.sigstore.json \
  --certificate-identity "https://github.com/runsheet/mesh-cli/.github/workflows/release-tag.yml@refs/tags/$TAG" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS
```

The identity names `mesh-cli`, not this repo — that is where the
workflow ran; the bytes here are the same ones it signed.

## Versions

`v0.2.x` continues the public line this repo has always published
under (the last of the old channel was `v0.2.186`). `v0.1.0` — the
first tag of `runsheet/mesh-cli`'s new signed channel, numbered from
its own private restart — is marked superseded here: a prerelease with
a note pointing at this line; its tag is left as it was. `mesh node
update` with no version follows the latest release here that is not a
prerelease.
