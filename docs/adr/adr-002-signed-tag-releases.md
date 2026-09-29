---
summary: Nodes update only from tag-cut releases of runsheet/mesh-cli whose SHA256SUMS is signed keyless by runsheet/mesh-cli release-tag.yml's GitHub OIDC identity (tags released before the repo left projectmesh-io also under the old identity); the asset names and that identity are the updater's contract. The same tag pushes and cosign-signs the node image ghcr.io/runsheet/mesh-node:<tag>, which the node pulls before it swaps. An upgrade swaps and restarts the node at once, and the new process adopts the sessions the old one left running; a sandboxed node alone still drains first.
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
  tag, and publishes a GitHub release on `runsheet/mesh-cli` (a
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
  `https://github.com/runsheet/mesh-cli/.github/workflows/release-tag.yml@refs/tags/<tag>`
  (and, for the tags released before the transfer only, the
  `projectmesh-io/mesh-cli` identity: see the transfer amendment below)
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
  `github.com/runsheet/mesh-cli/releases/download/<tag>/<asset>`,
  which works only while the repo is public. **It is private today**, and
  those URLs answer 404 even with a token (proven on the reference
  release, 2026-09-25), so a node cannot yet fetch a release on its own.
  Superseded by the amendment below (goal `signed-releases-on-runsheet-mesh`):
  the source moved to the public mirror `runsheet/mesh`, anonymous, no
  token, while the signing identity stays `runsheet/mesh-cli`'s.
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

## Amendment: the repository moved to runsheet (goal `mesh-cli-lives-in-runsheet`, 2026-09-26)

On 2026-09-26 12:40Z mesh-cli was transferred from `projectmesh-io` to
the `runsheet` GitHub organisation. GitHub redirects the old URLs, but a
workflow's OIDC identity names the repository it runs in, so every tag
released after the transfer is signed by
`https://github.com/runsheet/mesh-cli/.github/workflows/release-tag.yml@refs/tags/<tag>`,
which a verifier pinned to the old owner refuses. The contract above
(the identity and the URLs) is changed here first:

- `IdentityPrefix`, `ReleaseBaseURL` and `LatestReleaseURL` name
  `runsheet/mesh-cli`. The node image is `ghcr.io/runsheet/mesh-node:<tag>`:
  packages do not move with a repository, so it is published anew there
  by the moved repo's `release-tag.yml`, and `sandbox.DefaultImageRepo`
  follows once it is (a change of its own in the same goal).
- `Policy` carries a list of accepted identities, each an exact SAN
  match. `ReleasePolicy(tag)` always accepts the runsheet identity and
  adds `PreTransferIdentityPrefix` +
  tag — `https://github.com/projectmesh-io/mesh-cli/.github/workflows/release-tag.yml@refs/tags/<tag>`
  — only for a tag in the constant set of tags released before the
  transfer. That set is closed and, like the prefixes, is a constant:
  nothing in `node.yaml`, flags or the environment reaches it. The tags
  it holds:
  - `v0.0.0-selfupdate-test.1` (commit `97fb5fb`, released 2026-09-25),
    the last and only tag released under `projectmesh-io`.
- Why keep the old identity at all: those releases were signed before
  the move and cannot be re-signed without re-tagging, and the reference
  release's committed bundle (`internal/selfupdate/testdata/`) is one of
  them. Why only for those tags: after the transfer no one can run a
  workflow as `projectmesh-io/mesh-cli` unless the name is taken again —
  anyone who creates a repository there could then mint that identity
  for any new tag. Limiting it to tags that already exist means a
  recreated `projectmesh-io/mesh-cli` can sign nothing a node accepts.

### Transfer fixes

The first tags cut from `runsheet/mesh-cli` (`v0.0.0-runsheet-test.1`
to `.4`) failed in `release-tag.yml` for reasons that had nothing to do
with the identity; the first green run is `v0.0.0-runsheet-test.5`,
run 36247714213. Four changes, none to the contract above:

- **The image checks nftables by its package.** `nft --version` opens
  a netlink socket, which the linux/arm64 half built under qemu-user
  has not got (exit 3, `.1`); `images/node/Containerfile` runs
  `dpkg-query -W nftables` instead.
- **Publish downloads only the binaries.** build-push-action uploads
  the image's `.dockerbuild` build record as an artifact, which
  download-artifact could not fetch (`.2`); publish downloads with
  `pattern: mesh-*`.
- **The image job verifies what it signed.** After `cosign sign` it
  runs `cosign verify ghcr.io/runsheet/mesh-node@<digest>` against this
  identity and issuer, logged in to ghcr with `GITHUB_TOKEN`, so the
  package verifies while it is private.
- **Each image job has a docker config of its own.** `.4` (run
  36245651812) failed at `cosign sign` with `UNAUTHORIZED` though `.3`
  signed: the mini's runners share one `$HOME`, and `.3`'s
  docker/login-action post-step `docker logout ghcr.io` (13:53:49)
  removed the credential from `~/.docker/config.json` while `.4` was
  still building; its sign (13:55:18) asked ghcr anonymously. Not the
  login's scope and not signing by tag — it signed by digest. The job
  now sets `DOCKER_CONFIG` to `$RUNNER_TEMP/docker`, the buildx plugin
  linked in, so concurrent tags no longer log each other out.

The first tag cut from main with all four fixes is
`v0.0.0-runsheet-test.6` (`184011c`, run 36249480345, green, its image
`cosign verify`ed under the runsheet identity). `mesh node update
--verify-only` accepts it and the pre-transfer `v0.0.0-selfupdate-test.1`.
The stale `.1` to `.5` tags and releases are gone.
[releases-after-transfer.md](../releases-after-transfer.md) is the
record. `ghcr.io/runsheet/mesh-node` refuses anonymous pulls, as the
`projectmesh-io` package did: a sandboxed node pulls it only once logged
in to ghcr, or once the operator makes it public.

## Amendment: update.json names why a start or a confirm failed (goal `upgrades-survive-a-slow-platform`, 2026-09-28)

On 2026-09-28 the mini's update to `0a04088` rolled back with `update.json`
saying only `'0a04088 did not start; rolled back to 72ff910'`, and
node.log had nothing of the new process's own: the platform was slow (a
load of 200, every gateway call timing out) and `nodeStart`'s 60-second
registration timeout (`regCtx`) made the process exit before it ever
reached the loop or logged a line. A busy box or a platform blip
therefore looked exactly like a binary that could not run, and there was
nothing in the logs to tell them apart. Two changes, both local, no
platform call:

- **`selfupdate.State` gains `startError` and `confirmError`.**
  `StateFile.NoteStartFailure(reason)` records why the last start of the
  pending update ended before it could run — a config it could not read,
  a panic, an exited process; `NoteConfirmError(reason)` records the
  last error a heartbeat hit while the update is unconfirmed — a refusal
  or a timeout. Neither ends the pending update or changes `Pending()`:
  the next start gets its own chance. `Confirm` clears both.
  `Rollback(exePath, reason string, window time.Duration)` now takes the
  confirm window as a parameter and, when `reason` is empty, names the
  real cause: `'<to> did not start: <startError>'` when a start error was
  recorded, `'<to> did not confirm within <window>: <confirmError>'` when
  a confirm error was, else the original bare wording. `node.CheckStart`'s
  `StartCheck` carries `Pending` and `Attempts` so a caller can say which
  start this is.
- **node.log's first lines can no longer be silent.** `node.StartLines`
  logs the version, the pid and the config path (not the config itself —
  it may be what fails to load) and either `'confirming update <from> →
  <to>: start <n> of 3, rolls back on the third'` or `'no update
  pending'`, immediately after `startUpdates` and before `LoadConfig`,
  `platformClient` or any other platform call. `nodecmd.go`'s
  `registerNode` is everything a start needs before the loop exists
  (load node.yaml, check the accounts' secrets, the platform client, the
  work root, registration); an error there, or a panic, while an update
  is pending is recorded with `NoteStartFailure` before it is returned
  (a panic is re-raised after recording) — the 60-second registration
  timeout above is exactly such an error, and now names itself instead
  of vanishing.
- node.yaml's new `update.confirm_within` (`Config.Update.ConfirmWithin`,
  read through `Config.ConfirmWithin()`) is how long a new process keeps
  trying to confirm an update before it gives up; left out, it defaults
  to 5 minutes (`selfupdate.DefaultConfirmWithin`). This amendment wires
  the data model and the config key alone — the loop's own heartbeat retry
  against the window, and `NoteConfirmError`'s call site, are a later
  item of the same goal (`upgrades-survive-a-slow-platform`): only a
  failure to *run* counts as "did not start" once that lands; a
  heartbeat that times out within the window is retried, not rolled
  back on.

## Amendment: the confirm window retries (goal `upgrades-survive-a-slow-platform`, 2026-09-28)

The window the amendment above configures is now in force. It opens at
process start, and it is the only thing a slow platform can exhaust:

- **Registration retries.** While an update is pending
  (`StartCheck.Pending`), `nodeStart`'s registration (sign-in,
  `ResolveScopes`, `registerIn`, 60 s per attempt as before) is retried
  on the heartbeat interval until the window elapses. Each failure is
  logged `'self-update: confirming <to>: register node: <err>; retrying
  for <left> more'` and recorded with `NoteConfirmError` — not
  `NoteStartFailure`: a registration that times out is the platform
  being slow, and the log says so (`(the platform is slow)` after a
  timeout or deadline). With no update pending registration fails at
  once, as before.
- **Heartbeats retry.** The loop knows the update it is confirming
  (`Loop.Confirming`, `ConfirmWithin`, `Started`). Until a heartbeat goes
  through, each `HeartbeatNode` failure is logged `'self-update:
  confirming <to>: heartbeat <org>: <err>; retrying for <left> more'`
  and recorded with `Updater.NoteConfirmError` (`ReleaseUpdater`
  delegates to the state file). The first heartbeat through confirms,
  as before.
- **An elapsed window is `ErrUnconfirmed`.** With no heartbeat through by
  `Started + ConfirmWithin`, `Run` (or the registration) returns an
  `*UnconfirmedError` naming the version, the window and the last error;
  `endRun` logs `'self-update: <to> did not confirm within <window>:
  <err>; exiting for the service to start it again'` and the process
  exits 1, its sessions left to adopt. The service starts it again; the
  third such start rolls back, and `update.json`'s `lastError` is
  `'<to> did not confirm within <window>: <err>'` (`CheckStart` passes
  node.yaml's window, when it reads, to `Rollback`).
- **What counts as "did not start"** is a failure to run: a config it
  cannot read, an account secret it cannot read, a panic, an exit before
  the window — `NoteStartFailure`. A refused or timed-out platform call
  inside the window is never one.
- `mesh node upgrade` prints, after the restart, the window
  (`update.confirm_within`), the log to watch (`~/.mesh/node.log`) and
  that a binary that cannot run, or cannot confirm on three starts, is
  rolled back with `~/.mesh/update.json` saying why.

## Amendment: the public mirror on runsheet/mesh (goal `signed-releases-on-runsheet-mesh`, 2026-09-28)

The `projectmesh.io/mesh` repo the old, unsigned `cli-release.yml`
published to became `runsheet/mesh` — public — on 2026-09-28 12:00Z.
Every tag release is now published twice: the one above on
`runsheet/mesh-cli`, and an identical one on `runsheet/mesh`, so the
public download channel every install instruction points at is signed
again and current.

- `release-tag.yml`'s `publish` job computes `$NOTES` once, naming the
  source commit and both repos, and both creates use it. After it
  creates the release on `runsheet/mesh-cli` (`--verify-tag`, the tag
  already exists there), `scripts/mirror-release.sh <tag> <sha>
  <release-dir>` creates the same release on `runsheet/mesh` — same
  tag, title, notes, `--prerelease` when the tag has a `-`, the same
  five files — with no `--verify-tag`: the tag does not exist on the
  mirror, and GitHub creates it there on the mirror's default branch,
  as `cli-release.yml` did for v0.2.1–186. It then proves the mirror
  identical — the downloaded `SHA256SUMS` byte-for-byte the one just
  signed, its bundle still verifying against the **`runsheet/mesh-cli`
  identity** (the bytes are the same; the mirror is not itself a
  signing identity) — and pushes `dist/mirror/README.md` to the
  mirror's `README.md`, only when its content differs. A failing
  mirror step fails the workflow: there is no release on
  `runsheet/mesh-cli` alone.
- **The token.** The workflow's own `GITHUB_TOKEN` cannot write another
  repo. `RELEASE_TOKEN` — the repo secret that published the old
  `cli-release.yml` channel to this same public repo before its
  transfer — is reused: a fine-grained token scoped to `runsheet/mesh`
  with Contents: read and write, or the runsheet org App's installation
  token.
- **Numbering continues the public line.** The next tag is
  **`v0.2.187`**, the first signed one on the mirror; `runsheet/mesh-cli`
  itself restarted numbering below it at `v0.1.0` (the transfer
  amendment above), which is superseded — a prerelease with a note
  pointing at the `v0.2.x` line, its tag left as it was. Goal
  `node-releases-on-merge`'s auto-tag takes max `v0.2.x` on
  `runsheet/mesh` plus one, not `runsheet/mesh-cli`'s own tags: the
  mirror is the public line's record.
  `docs/releases-after-transfer.md` carries the detail.
- The mirror's `README.md` is a source file here,
  `dist/mirror/README.md`, published verbatim: what the repo is, how to
  install, and how to verify a download against the
  `runsheet/mesh-cli` identity — since that is where the workflow ran,
  not the mirror.
- `internal/selfupdate` is unchanged by this amendment: the updater
  still downloads from `runsheet/mesh-cli` and verifies against its own
  identity. Which repo `selfupdate.Source` downloads from is a
  following item of the same goal.

## Amendment: the node downloads from runsheet/mesh (goal `signed-releases-on-runsheet-mesh`, 2026-09-28)

`runsheet/mesh-cli` stays private, so the source above (`selfupdate.GitHub`
downloading from it directly) never worked outside a token an operator
happened to hold. The public repo that used to carry mesh-cli's releases
(`projectmesh-io/mesh`, 186 unsigned `v0.2.x` releases through
`v0.2.186`) was transferred to `runsheet/mesh` on 2026-09-28 and
`release-tag.yml` now publishes each signed tag there too (a sibling goal
covers the workflow and the version numbering); this amendment is only
the updater's side of that:

- `selfupdate.ReleaseBaseURL` and `LatestReleaseURL` name `runsheet/mesh`:
  the node, `mesh node update` and `mesh node upgrade` all download and
  ask for the latest release anonymously from the public mirror, no
  token. `Fetch`'s 404 reason changes to name it: `release %s has no %s
  on runsheet/mesh (404: not published there, or not a signed release)`.
- The signing identity is unchanged: `verify.go`'s `IdentityPrefix`,
  `PreTransferIdentityPrefix`, `preTransferTags` and `ReleasePolicy` are
  exactly as the Decision above left them. The mirror holds the same
  bytes `runsheet/mesh-cli`'s `release-tag.yml` signed; the updater
  verifies against that workflow's identity whichever repo the bytes
  came from, so a fork or an unrelated push to `runsheet/mesh` proves
  nothing.
- `mesh node update` with no version takes the mirror's latest
  non-prerelease. Until `v0.2.187` is published there, that is still
  `v0.2.186` — the last of the old unsigned `cli-release.yml` builds,
  carrying no `SHA256SUMS` or Sigstore bundle at all. `Fetch` refuses it
  with `ErrMissingAsset` exactly as it would any other release missing
  an asset; this is the intended behaviour, not a bug to work around,
  and it clears itself the moment `v0.2.187` ships.
- This supersedes the Decision section's private-repo bullet above: the
  choice it left open (make the repo public, or move to an authenticated
  download) is resolved by the mirror instead — the source repo stays
  private, and the public copy of its releases is what a node reads.

## Consequences

- Renaming an asset, the workflow file, or the repo breaks every
  node's updater; each is a contract change made here first.
- Only a tag push can produce an acceptable signature, so who may push
  `v*` tags is who may ship to nodes.
- darwin/amd64 and windows are not release targets; a node there cannot
  self-update.
- A tag that cannot build its image publishes no release: a toolchain
  bump in the Containerfile is tested by the tag like the binary is.
