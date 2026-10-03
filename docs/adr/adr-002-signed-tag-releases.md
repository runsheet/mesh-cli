---
summary: Nodes update only from tag-cut releases of runsheet/mesh-cli whose SHA256SUMS is signed keyless by runsheet/mesh-cli release-tag.yml's GitHub OIDC identity (tags released before the repo left projectmesh-io also under the old identity); the asset names and that identity are the updater's contract. The same tag pushes and cosign-signs the node image ghcr.io/runsheet/mesh-node:<tag>, which the node pulls before it swaps. An upgrade swaps and restarts the node at once, and the new process adopts the sessions the old one left running; a sandboxed node alone still drains first. Every merge to main that changes cmd/, internal/, images/node/ or go.mod/go.sum is tagged automatically by release-on-merge.yml, pushing with RELEASE_TAG_TOKEN.
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
  token. **Superseded by
  [ADR-015](adr-015-workflow-tokens-exchanged-through-rigger.md)**
  (goal `release-tag-mirrors-with-a-rigger-binding`, 2026-09-30): the
  mirror step exchanges the run's OIDC identity (audience
  `runsheet-rigger`) at the runsheet gateway for a token on
  `runsheet/mesh` alone, `contents: write`, at most an hour
  (`scripts/exchange-workflow-token.sh`), against the binding the mirror
  carries, `.runsheet/bindings/mesh-cli-release.yaml` — published from
  `dist/mirror/` as the README is. `RELEASE_TOKEN` stays only as the
  logged fallback (`mirror token: RELEASE_TOKEN fallback (exchange
  failed: <reason>)`) until the runsheet half answers; the switch is the
  operator running `gh secret delete RELEASE_TOKEN --repo
  runsheet/mesh-cli` once a run reads `exchanged`, not a PR (ADR-015,
  "Where it stands").
- **Numbering continues the public line.** The public line continued
  at `v0.2.187` (cut and signed on `runsheet/mesh-cli` only, its mirror
  step failed), and **`v0.2.188`** was the first signed release on the
  mirror; `runsheet/mesh-cli`
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

**What happened:** `v0.2.187` was cut on 2026-09-28 at 22:46Z
(`909202b`) and signed and released on `runsheet/mesh-cli`, but it
never reached the mirror. The mirror step of run `36494313388` failed
in `gh release create` itself, because the tag existed only on the
source ("tag v0.2.187 exists locally but has not been pushed to
runsheet/mesh … specify the `--target` flag"). #336 fixed it with
`--target main`. `v0.2.188` (`5a8c29e`) was the first tag cut with that
fix. Attempt 1 of its run, `36499785802`, signed and released it on
`runsheet/mesh-cli`. Then the mirror refused it: `HTTP 403: Resource
not accessible by personal access token`, because `RELEASE_TOKEN` had
no access to `runsheet/mesh`. The token was replaced. The fine-grained
token must be owned by `runsheet`, scoped to `runsheet/mesh` alone,
with Contents: read and write. On attempt 3, on 2026-09-29 at 00:50Z,
`v0.2.188` reached `runsheet/mesh` as the first signed release there.
It carries the same assets as the source, a `SHA256SUMS` that `cmp`
finds equal and that verifies against the `runsheet/mesh-cli`
identity, and the mirror's README. At 00:52Z the mini
updated to the mirror's `mesh-linux-amd64` through the operator's
`mesh node upgrade`. Its planner sessions were adopted, and the update
was confirmed. The numbering
rule (max `v0.2.x` on `runsheet/mesh` plus one) next yields
**`v0.2.189`**. `docs/releases-after-transfer.md` ("v0.2.188: the first
release on the mirror") is the record.

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
  non-prerelease, `v0.2.188` or later. Releases `v0.2.1`–`v0.2.186`
  there are the old unsigned `cli-release.yml` builds and carry no
  Sigstore bundle. `Fetch` refuses them with `ErrMissingAsset`, as it
  would any other release missing an asset. `v0.2.187` exists on
  `runsheet/mesh-cli` only, so the mirror answers 404 for it.
- This supersedes the Decision section's private-repo bullet above: the
  choice it left open (make the repo public, or move to an authenticated
  download) is resolved by the mirror instead — the source repo stays
  private, and the public copy of its releases is what a node reads.

## Amendment: every node-changing merge is a release (goal `node-releases-on-merge`, 2026-09-28)

The mini ran a build ten commits behind main because nothing between a
merge and a node made a release: a person cross-built and ran `mesh
node upgrade --binary`, and the only releases were test prereleases
until `v0.1.0`. The path is now automatic, and the by-hand tag is the
fallback:

**merge to main → `release-on-merge.yml` → tag `vX.Y.Z` on the merge
commit → `release-tag.yml` → signed release (and the mirror) → the
console offers it.**

- **What counts as node-changing.** A push to `main` whose changes
  (against the first parent) touch `cmd/**`, `internal/**`,
  `images/node/**`, `go.mod` or `go.sum`. The workflow's `paths` filter
  says so, and `scripts/release-on-merge.sh` checks it again itself, so
  a direct call on a docs-only commit prints `no release: …`. Docs,
  specs, the README and `.github` alone cut no release.
- **The number.** The last release is the highest `vX.Y.Z` (no `-`:
  test prereleases never count) among this repo's tags and the public
  mirror's (`runsheet/mesh`, whose `v0.2.x` line this repo continues,
  per the mirror amendment above); none is `v0.1.0`. The bump is the
  patch. A commit already carrying a `vX.Y.Z` tag is no release, so a
  re-run or a tag cut by hand never makes a second one.
- **Minor is asked for**, patch reset to 0, by the merged PR's label
  `release:minor` (the PR found by `gh api
  repos/<repo>/commits/<sha>/pulls`, else the squash subject's
  `(#NNN)`), or by `release: minor` in the frontmatter of a
  `specs/goal-*.md` the merge touches. Major is by hand only. The label
  is the operator's to create once (`gh label create release:minor`).
- **The tag-push credential is the shipping credential.** The tag is
  pushed with the repository secret `RELEASE_TAG_TOKEN` — a fine-grained
  token with Contents: read and write on `runsheet/mesh-cli`, which the
  operator creates; the workflow fails naming it when it is unset. Not
  `GITHUB_TOKEN`: GitHub starts no workflow for a push made with it, so
  its tag would never run `release-tag.yml`. And `release-tag.yml` is
  not called as a reusable workflow: a called workflow signs under the
  caller's run with its own `job_workflow_ref`, not the
  `release-tag.yml@refs/tags/<tag>` identity `ReleasePolicy` pins. So
  `release-tag.yml` stays the only signer, unchanged, and "who may push
  `v*` tags is who may ship to nodes" (Consequences) now reads: who
  holds `RELEASE_TAG_TOKEN`, or may merge node-changing code to `main`.
- `concurrency: release-on-merge` (never cancelled) serializes
  back-to-back merges, so each sees the tag the one before pushed.
  `workflow_dispatch` with `dry_run` (the default) prints the next tag
  for its ref's head and pushes nothing.

## Amendment: version is tag-or-sha, and the heartbeat carries commit (goal `node-releases-on-merge`, 2026-09-28)

Every node-changing merge to main is now a release (mesh-specs ADR 013),
and the console shows which one a node runs and how far behind it is.
A binary built outside release-tag.yml reported `dev`, which says
nothing about what it is; the mini ran 72ff910 for a day with the
console none the wiser.

- `internal/version` gains `Sha` (an ldflag, like `Version`) and
  `Commit()`: that ldflag, else the Go toolchain's `vcs.revision`
  cut to 7 characters with `-dirty` when `vcs.modified`, else empty.
  `Resolved()` is what the node reports itself as everywhere — register,
  heartbeat, node.log's start lines, `mesh node update`/`upgrade`'s
  running version, the channel MCP server's `serverInfo`, `mesh
  --version`: `Version` when it is a tag, else the commit, else `dev`.
  `mesh version` prints both: `mesh v0.2.188 (commit 0895d40)`, or
  `mesh 0895d40 (dev build)`.
- release-tag.yml sets `-X …/internal/version.Sha=${GITHUB_SHA::7}`
  beside `Version=${GITHUB_REF_NAME}`, so a release knows its commit
  even under `-trimpath` or with vcs stamping off.
- The heartbeat carries `commit` (`HeartbeatExtras.Commit`, filled from
  `Loop.Commit`) beside `version` — on the loop's heartbeats, the
  confirm window's among them, and `mesh node status`'s one — sent only when set and only when the
  gateway's `HeartbeatNodeInput` has the field — introspected and kept
  for an hour exactly as `scaling` is. A platform without it gets the
  heartbeat it always did.
- `mesh node upgrade --binary <path>` reads `<path> version` as any of
  its shapes — the tagged, the untagged, the dirty (`mesh 0895d40-dirty
  (dev build)`) and an older binary's bare `mesh <version>` — and takes
  the version alone, never the parenthesis.
- Self-update is unchanged in effect: only a node reporting a release
  (`version.Release`: `v` then a digit) follows `desiredVersion`. A
  sha-versioned dev build was put there by hand from a checkout and is
  left alone, as `dev` always was; a desired version equal to the
  running tag still does nothing.

## Amendment: the publish job is idempotent (goal `release-tag-mirrors-with-a-rigger-binding`, 2026-09-29)

`gh release create` refuses outright when a release of the tag already
exists ("release already exists"), so a re-run of the publish job after
a failure partway through — the mirror step, say — had no way back:
`v0.2.188`'s attempt 1 (run `36499785802`, the "What happened" section
above) signed and released on `runsheet/mesh-cli`, then failed at the
mirror; attempt 3 could not re-run `gh release create` on
`runsheet/mesh-cli` at all, and the release there was deleted by hand
to unblock it. Both creates — `release-tag.yml`'s own "Create release
on runsheet/mesh-cli" step and `scripts/mirror-release.sh`'s create on
the mirror — now go through `scripts/ensure-release.sh <tag> <sha>
<release-dir>` in place of a bare `gh release create`:

- **No release of the tag on `GH_REPO`:** created exactly as before —
  `--verify-tag` on `runsheet/mesh-cli` (the tag is already pushed
  there), `--target main` on the mirror (it is not), `--prerelease`
  when the tag has a `-`, the title, the shared `$NOTES` and the five
  assets.
- **An existing release whose `SHA256SUMS` is byte-for-byte
  `<release-dir>/SHA256SUMS` and whose assets include the three
  binaries: kept.** Nothing is created, deleted or overwritten on
  GitHub; the log says `release <tag> already on <repo> with identical
  assets; keeping it`. A re-run after a failure downstream is a no-op
  here. On `runsheet/mesh-cli` only, the existing release's
  `SHA256SUMS.sigstore.json` is downloaded over `<release-dir>`'s
  freshly-signed one, so the mirror step that follows publishes the
  exact bytes the source release already carries rather than a second,
  differently-signed one — keyless signing mints a new Sigstore
  certificate every run, so a re-signed bundle is valid but not
  identical; the existing bundle still verifies.
- **An existing release whose `SHA256SUMS` differs, or that is missing
  one of the three binaries: refused.** `ensure-release.sh` exits
  non-zero naming the asset and the repo; nothing already published is
  touched. A mismatch this deep — the tag exists but does not match
  what this run built — is for a person to resolve, not to overwrite
  automatically.
- **Nothing is ever deleted**, on either repo, by this workflow. The
  by-hand deletion that unblocked `v0.2.188` is no longer how a re-run
  after a partial failure is fixed.

`scripts/mirror-release_test.sh` covers both call sites (a fake `gh`
answering `release view --json assets` from `FAKE_` variables): no
release creates as before, an identical existing release is kept with
no create and no delete, and a differing one exits non-zero with no
create — on the mirror (`GH_REPO=runsheet/mesh`) and on the source
(`GH_REPO=runsheet/mesh-cli`, `--verify-tag` instead of `--target`)
alike.

## Amendment: a rollback on a refusal naming the owner id is not final (goal `a-node-takes-the-id-the-platform-names`, 2026-10-03)

On 2026-10-03 the grapevine node's update to v0.2.248 rolled back with
`v0.2.248 did not confirm within 5m0s: register (owner person):
RegisterNode: node grapevine registers at
18a8b766-18f3-5e73-b4cf-efb91c01946a, the id its form names, not
47a08dd9-…`, and the rule above — a rolled-back version is not tried
again until the platform asks for another — left it on v0.2.247 for
good. The binary was fine; the id it registered under was not, and the
platform had named the right one. ADR-015's 2026-10-03 section has the
node adopt that id and keep it in node.yaml (`node_id`) before the
rollback, so the same version would now register and confirm. The
rollback rule gains one exception:

- **`update.json` carries `retryAfterAdopt`.** `StateFile.NoteConfirmError`
  sets it when the reason names an id — `registers at <uuid>`,
  `selfupdate.NamesID`, parsed in the package itself, which imports
  nothing from `internal/node` — and a later reason naming none does
  not clear it. `Rollback` keeps it; `BeginUpdate` and `Confirm` clear
  it. The confirm window records each refused registration's own text
  (`confirmWindow.retry` → `NoteConfirmError(err.Error())`), so the id
  is in `confirmError` and in the rollback's `lastError`.
- **Such a rollback is not final.** `node.CheckStart` gives
  `StartCheck.Final = rolledBackAt set && !retryAfterAdopt`, and when
  the flag is set `StartCheck.RetryNow` and a `Reason` ending ` (owner
  id adopted; the update is tried again)` — the heartbeat's
  `updateError` says so. `Loop.NoteUpdateFailure(version, reason, final,
  retryNow)` seeds the loop, and `selfUpdate` skips the `updateRetry`
  wait for that version once: the next tick that reads it as the
  desired version starts the update again. That attempt is a fresh
  `BeginUpdate`, so a second rollback of it — whatever it names — is
  final as before.
- The exception helps only a rollback read by a binary that has it: the
  binary that rolls back writes the flag, but the previous binary reads
  it, so v0.2.247 still treats its v0.2.248 rollback as final; the
  release after it is a new desired version and is tried anyway.

Tests: `internal/selfupdate/state_test.go`,
`internal/node/selfupdate_adopt_test.go`,
`internal/commands/nodestart_test.go`
(`TestConfirmWindowRecordsTheRefusalNamingAnID`).

## Consequences

- Renaming an asset, the workflow file, or the repo breaks every
  node's updater; each is a contract change made here first.
- Only a tag push can produce an acceptable signature, so who may push
  `v*` tags is who may ship to nodes.
- darwin/amd64 and windows are not release targets; a node there cannot
  self-update.
- A tag that cannot build its image publishes no release: a toolchain
  bump in the Containerfile is tested by the tag like the binary is.
