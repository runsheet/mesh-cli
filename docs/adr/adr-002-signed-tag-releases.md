---
summary: Nodes update only from tag-cut releases of projectmesh-io/mesh-cli whose SHA256SUMS is signed keyless by release-tag.yml's GitHub OIDC identity; the asset names and that identity are the updater's contract. The same tag pushes and cosign-signs the node image ghcr.io/runsheet/mesh-node:<tag>, which the node pulls before it swaps. An upgrade swaps and restarts the node at once, and the new process adopts the sessions the old one left running; a sandboxed node alone still drains first.
status: accepted
---

# ADR-002: A node updates only from a signed tag release

## Context

Goal `node-self-update`: a node replaces its own executable with the
version the platform asks for. Whatever it downloads runs with the
node's credential and its operator's `gh`, so it must be able to prove,
with nothing but the release itself, that the binary was built by this
repo's workflow from that tag. `cli-release.yml` used to publish on
every push to `main`, to another repo (`projectmesh-io/mesh`), with a
run-number version and unsigned sums; it was dropped (see the amendment
below).

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
- `release.yml` stays as it is and is not an update source.
  `cli-release.yml` was dropped when the repo moved to the runsheet org
  (2026-09-26): its `GITHUB_TOKEN` cannot publish cross-org, and
  publishing main builds to this repo's releases would make them
  `latest` and break `LatestReleaseURL`. Nodes update only from
  `release-tag.yml`.
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
  release costs it no work — then records the update, swaps the
  resolved executable (the previous kept as `<path>.prev`) and `Run`
  returns `ErrRestartForUpdate` at once, without a drain: the sessions
  it has running are left in their own units and the new process
  adopts them (restart and adopt, below). `mesh node start` exits 0 and
  systemd (`Restart=always`) or launchd (`KeepAlive`) starts the new
  binary. A sandboxed node still drains first (`DrainAndWait`: every
  session of every kind ends), since its sessions' pods end with it. A
  refusal or a failed swap resumes a drain the update began, puts the
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

## Amendment: the node image (goal `sandboxed-sessions`, 2026-09-25)

The same tag also ships the node image every sandboxed session runs in
(mesh-specs ADR 007). The binary's contract above is unchanged — the same
five assets, the same `SHA256SUMS` signature, the same identity — and the
image is a second artefact beside it:

- `release-tag.yml`'s `image` job builds `images/node/Containerfile` for
  linux/amd64 and linux/arm64 (Go at go.mod's version, `MESH_VERSION`
  the tag), pushes `ghcr.io/runsheet/mesh-node:<tag>` and signs its
  digest keyless with `cosign sign`, under the same workflow identity.
  `publish` needs it, so no release exists without its image.
- The tag is the node's version as it is (`internal/version.Version`,
  `v1.2.0` → `mesh-node:v1.2.0`). Renaming the repository or dropping the
  `v` is a contract change made here first.
- Once a release is verified and before anything is drained — the
  loop's self-update and `mesh node update` alike — the node ensures the
  new version's image (`SandboxConfig.EnsureImage`): `podman pull` of the
  tag, then every other `mesh-node` tag removed but the running
  version's, which the node's sessions still use. A pull that fails is
  the update's error — `updateError` on the heartbeat, the command's
  exit — with no drain and no swap: the node stays on its binary, and the
  version waits `updateRetry` as a refused release does. Nothing falls
  back to a local build on its own; node.yaml's `sandbox.image: build`
  asks for one (below). `sandbox: off`, or no podman with no mode set,
  pulls nothing (mesh-cli ADR-007).
- node.yaml's `sandbox.image`: empty is `mesh-node:<version>`; `build`
  is that tag built on the node from `images/node/Containerfile`, which
  the binary embeds (`images/node`, `nodeimage.Containerfile`), with
  `sandbox.BuildImage`; any other value is used as given.
- The node does not yet verify the image's cosign signature on pull;
  the signature is there for when it does.
- The package is private while the repository is (the release caveat
  above): a pull needs `podman login ghcr.io` with a token that can read
  it, or `sandbox.image: build`.

## Amendment: restart and adopt instead of drain (goal `node-upgrades-keep-sessions`, 2026-09-26)

Draining for an upgrade waited for the longest session — hours for a
build — or, when the operator restarted instead, killed every one.
Since mesh-cli ADR-008 and ADR-009 a host session runs in its own unit
and a starting node adopts every session whose state file names it, so
an upgrade no longer needs either. **The update process is now: verify,
ensure the image, swap, restart at once, adopt.**

- **The loop's self-update** swaps as soon as the release is verified
  and its image ensured, then `Loop.detach` cancels every context the
  node runs sessions in (`Loop.Context`): each `Job.Outlive` session
  detaches, its unit and `<workdir>.session/` left in place, and `Run`
  returns `ErrRestartForUpdate` without waiting for any of them. The
  service starts the new binary, whose `FindSessions` →
  `AdoptSessions` heartbeats each item at once and follows it to its
  end. The start count and the rollback after two failed starts are
  unchanged: `Begin` is recorded before the swap and the first
  heartbeat confirms. A node whose sandbox is on (`Loop.Sandbox.Mode`)
  drains first as before: its pods are not yet their sessions' unit.
- **`mesh node upgrade`** (`internal/commands/nodeupgrade.go`) is the
  by-hand path: `--version vX.Y.Z` or, with neither flag, the latest
  release, fetched and verified on the same `ReleaseUpdater` and its
  image ensured; or `--binary <path>`, a local build installed once
  `<path> version` answers `mesh <version>`. Either is swapped keeping
  `.prev` and recorded for the rollback, then the node is restarted at
  once: `systemctl --user restart io.projectmesh.mesh-node.service`
  when that unit is active, `launchctl kickstart -k
  gui/<uid>/io.projectmesh.mesh-node` when the agent is loaded, else
  `SIGHUP` to the pidfile's node. It never drains. A sandboxed node is
  refused unless `--force`.
- **SIGHUP** makes a running node restart at once (`Loop.Restart`):
  every session detached as above, `Run` returns `ErrRestart`, and
  `mesh node start` execs its own resolved executable — by then the new
  binary — with the same arguments, keeping its pid.
- **The service never reaches a session.** The systemd unit carries
  `KillMode=process`, so a stop or restart signals the node's main
  process alone (a session's scope is another unit and cgroup anyway;
  a detached session in the service's cgroup is spared too), and the
  LaunchAgent carries `AbandonProcessGroup`. An installed unit takes
  them on `mesh node service install`.
- `mesh node update` still drains before its swap, and `mesh node
  drain` (`SIGUSR1`) is unchanged: draining is for taking a machine out
  of service.

## Consequences

- Renaming an asset, the workflow file, or the repo breaks every
  node's updater; each is a contract change made here first.
- Only a tag push can produce an acceptable signature, so who may push
  `v*` tags is who may ship to nodes.
- darwin/amd64 and windows are not release targets; a node there cannot
  self-update.
- A tag that cannot build its image publishes no release: a toolchain
  bump in the Containerfile is tested by the tag like the binary is.
