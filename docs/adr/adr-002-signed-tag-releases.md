---
summary: Nodes update only from tag-cut releases of projectmesh-io/mesh-cli whose SHA256SUMS is signed keyless by release-tag.yml's GitHub OIDC identity; the asset names and that identity are the updater's contract.
status: accepted
---

# ADR-002: A node updates only from a signed tag release

## Context

Goal `node-self-update`: a node replaces its own executable with the
version the platform asks for. Whatever it downloads runs with the
node's credential and its operator's `gh`, so it must be able to prove,
with nothing but the release itself, that the binary was built by this
repo's workflow from that tag. `cli-release.yml` already publishes on
every push to `main`, but to another repo (`projectmesh-io/mesh`), with
a run-number version and unsigned sums; changing it would change what
existing installs download.

## Decision

- A new workflow, `.github/workflows/release-tag.yml`, runs on a pushed
  tag `v*` and nothing else (no `workflow_dispatch`: a release is a tag).
  It builds linux/amd64, linux/arm64 and darwin/arm64 with
  `CGO_ENABLED=0 -trimpath` and `internal/version.Version` set to the
  tag, and publishes a GitHub release on `projectmesh-io/mesh-cli` (a
  prerelease when the tag has a `-`).
- The release carries exactly `mesh-linux-amd64`, `mesh-linux-arm64`,
  `mesh-darwin-arm64`, `SHA256SUMS` (`sha256sum mesh-*`) and
  `SHA256SUMS.sigstore.json`, the bundle from
  `cosign sign-blob --yes --bundle SHA256SUMS.sigstore.json SHA256SUMS`.
  With the cosign 2.x that `sigstore/cosign-installer@v3` installs, that
  is cosign's legacy bundle JSON (`base64Signature`, `cert`,
  `rekorBundle`), not the protobuf Sigstore bundle; the updater reads
  both (`parseBundle` turns the legacy one into a v0.1 bundle whose log
  entry carries Rekor's SET as its inclusion promise, and verifies it
  like any other), so adding `--new-bundle-format` to the workflow later
  needs no node change.
- Signing is keyless Sigstore: there is no key to hold or leak. The
  updater accepts the bundle only for certificate identity
  `https://github.com/projectmesh-io/mesh-cli/.github/workflows/release-tag.yml@refs/tags/<tag>`
  and issuer `https://token.actions.githubusercontent.com`, where
  `<tag>` is the version it asked for; then the binary for its own
  OS/arch must match its line in `SHA256SUMS`. Anything else is refused
  and reported.
- `cli-release.yml` and `release.yml` stay as they are and are not an
  update source.
- The updater (`internal/selfupdate`) verifies in-process with
  `github.com/sigstore/sigstore-go`; the node needs no `cosign`. It
  requires a transparency-log entry, an observer timestamp and an
  embedded SCT, chains to Sigstore's public-good trusted root (fetched
  over TUF from the root embedded in sigstore-go, cached in
  `~/.mesh/sigstore`), and matches the identity and issuer exactly. The
  identity prefix and issuer are constants in `ReleasePolicy`; nothing in
  `node.yaml`, flags or the environment can loosen them. A refusal is one
  of `ErrMissingAsset`, `ErrSignature`, `ErrIdentity`, `ErrChecksum`
  with a short reason for the heartbeat; the update's progress and last
  error live in `~/.mesh/update.json`.
- `selfupdate.GitHub` downloads anonymously from
  `github.com/projectmesh-io/mesh-cli/releases/download/<tag>/<asset>`,
  which works only while the repo is public. **It is private today**, and
  those URLs answer 404 even with a token (proven on the reference
  release, 2026-09-25), so a node cannot yet fetch a release on its own.
  Either the repo (or a public mirror of its releases) goes public, or
  the source moves to the authenticated API asset download with a
  credential the node holds; that choice is open. Until then an operator
  can `gh release download` and run `mesh node update --from-dir`.
- The loop follows `NodeIndex.desiredVersion` (the platform half,
  update-nodes-from-console), read after each tick's heartbeat
  (`GetDesiredVersion`; a platform without the field is no update,
  logged once). Empty, the running version, or a `dev` build do
  nothing. Otherwise the node fetches and verifies *first* — a refused
  release costs it no work and no drain — then drains (`DrainAndWait`:
  every session of every kind ends), records the update, swaps the
  resolved executable (the previous kept as `<path>.prev`) and `Run`
  returns `ErrRestartForUpdate`: `mesh node start` exits 0 and systemd
  (`Restart=always`) or launchd (`KeepAlive`) starts the new binary. A
  refusal or a failed swap resumes the drain the update began, puts the
  reason on every heartbeat (`updateError`) while that version is still
  asked for, and leaves that version for an hour. A platform that
  refuses `updateError` gets the basic heartbeat instead (version,
  running, draining), and the node offers the field again every ten
  minutes (`HeartbeatNode`).
- Every `mesh node start` counts itself against a pending update; the
  third unconfirmed start (the new binary failed to start twice) puts
  `.prev` back, heartbeats the reason as the old version, and exits 0
  for the old binary to start. A rolled-back version is not tried again
  until the platform asks for another. The first heartbeat that goes
  through confirms the update.
- `mesh node update [version]` does the same by hand, with the same
  `ReleaseUpdater` and `selfupdate.GitHub`: no version is the latest
  release that is not a prerelease (GitHub's `releases/latest`). It
  fetches and verifies first under `ReleasePolicy` — a refusal prints
  its reason, exits non-zero and changes nothing, not even
  `update.json` — then drains the node running on the machine (the
  pidfile's `SIGUSR1`, as `mesh node drain`; none running is nothing to
  drain), records the update and swaps keeping `<path>.prev`, so the
  start count and rollback above hold for it too. The swap follows the
  drain signal at once: a node with no session exits on it, and its
  service waits 10 seconds (`RestartSec`, `ThrottleInterval`) before
  starting what is by then the new binary. `--verify-only` downloads and
  verifies into a temporary directory and swaps nothing. `--from-dir`
  reads the release's files from a directory (`selfupdate.Dir`) instead
  of GitHub and verifies them under the same `ReleasePolicy` of the
  version named, so a directory cannot vouch for itself; it is how an
  edited `SHA256SUMS` is shown to be refused. It needs no
  platform, so a node the console cannot reach can still be updated.
- Test tags carry a `-` (`v0.0.0-selfupdate-test.<n>`), so they are
  prereleases and never the `latest` a node picks up unasked. The
  reference release, `v0.0.0-selfupdate-test.1`, keeps its `SHA256SUMS`
  and bundle in `internal/selfupdate/testdata/`, with Sigstore's
  public-good trusted root of the day, and the package's tests verify
  them offline against the real identity and refuse them under another
  tag's or with edited sums.

## Consequences

- Renaming an asset, the workflow file, or the repo breaks every
  node's updater; each is a contract change made here first.
- Only a tag push can produce an acceptable signature, so who may push
  `v*` tags is who may ship to nodes.
- darwin/amd64 and windows are not release targets; a node there cannot
  self-update.
