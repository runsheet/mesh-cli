---
summary: How mesh-cli implements mesh-specs ADR 007 — the node image carrying Postgres and headless Chromium beside Go, Node and Python (scripts/image-tools-check.sh run in it by TestImageToolsE2E), internal/sandbox's Runtime (Podman, a test Fake) runs claude in a per-session pod with two mounts, an allow-listed env passed to podman by name, node.yaml's flat sandbox keys (mode, cpus, memory, pids, services_cpus, services_memory, registries, image), a mode left out following `podman --version` while an explicit on fails closed at start, a limit kill (OOM, pids, the runtime's timeout backstop) released as the node's condition with the heartbeat's reason, service sidecars started and health-checked in the session's pod before the agent (a required one failing releases the claim, an optional one is a note), the node image (images/node/Containerfile, embedded) pulled for the new version before a self-update swaps or `mesh node update` drains — a failed pull an updateError with no drain and no swap, the pull on a registry credential the platform brokers (`nodeRegistryToken`) written to a temporary auth.json passed by --authfile and removed after, `sandbox.image: build` building it locally — the pod mapping the host user onto the image's declared user (`--userns keep-id:uid=<image uid>,gid=<image gid>`, 1000 when it declares none; podman 4.3 or later), a session's HOME, pod, containers and volumes removed on every way it ends — `Session.Close` sure of the HOME (RemoveAll, then u+w, then `podman unshare rm -rf`) and logging what it could not remove — and a node start sweeping (`sandbox.Sweep`) the mesh-home dirs no running container mounts, the mesh pods with no running container and the exited mesh containers an earlier process left, a sandboxed Outlive session's pod its unit — the agent run detached (`podman run -d`, streams on `<workdir>.session/` files), `state.json` naming the pod, container and HOME, the pod and HOME left standing when the node lets go, kept by the start-up sweep, and reattached by the adopting node, so a sandboxed node's self-update no longer drains — and channel mode and Codex not yet sandboxed.
status: accepted
---

# ADR-007: The sandbox on the node

## Context

mesh-specs ADR 007 decides that every session runs in its own rootless
Podman pod, with its workspace and a temporary HOME the only mounts, an
allow-listed environment, limits from node.yaml, and a network that
refuses the LAN and the host. This records the choices mesh-cli made
implementing it that the spec leaves open or that differ in detail.

## Decision

- **One package, one interface.** `internal/sandbox` has `Runtime`
  (`Version`, `CreatePod`, `RunAgent`, `AddSidecar`, `ExecSidecar`,
  `ProbeTCP`, `Remove`), a
  `Podman` that shells out to `podman` one call per step, and a `Fake`
  that records the specs for tests. `Sandbox.Open` makes the session's
  HOME, `Session.Run` creates the pod, runs the agent and removes the
  pod, `Session.Close` removes anything left and the HOME.
- **Where it plugs in.** `harness.ClaudeCode.Sandbox`: nil is the old
  exec path, untouched; set, the harness writes the MCP config to
  `$HOME/mcp.json` (the mesh-context bearer on no command line), and runs
  `claude` with the same arguments otherwise in the pod. Every one-shot
  session (poll-mode build, evaluate, plan, research, rebase) goes
  through it. Channel-mode sessions and Codex are later items of the
  goal.
- **Env by name.** The container's variables are given to `podman run`
  as `--env NAME` with the values in podman's own process environment,
  and `--unsetenv-all` drops the image's and podman's defaults, so no
  secret is on a command line and nothing else reaches the agent.
  Podman's process keeps only what it needs to find its storage —
  the node's `podmanPassthrough` values, `PATH`, `HOME`, `USER`,
  `LOGNAME`, `XDG_*`, `CONTAINERS_*` and `TMPDIR` — and keeps the
  node's whatever the container sets: rootless podman finds its store
  by `HOME` and `XDG_*`, and a session's temporary `HOME` there sent
  podman to an empty store and a pull it could not make (the dl360,
  2026-10-10; goal podman-keeps-the-nodes-home). A container variable
  with a passthrough name reaches the container as `--env NAME=value`
  instead (`envArgs`; its value is a path, never a secret), and every
  other variable stays a bare `--env NAME` read from podman's
  environment (`podmanEnv`).
- **node.yaml keys are flat**: `sandbox.{mode, cpus, memory, pids,
  services_cpus, services_memory, registries, image, service_timeout}` rather than the
  spec's nested `limits {…}` / `services_budget {…}`; `sandbox: off`
  alone is the mode.
- **The mode.** `off` never asks the runtime. Left out, it follows the
  runtime: on when `podman --version` answers, off with one log line
  when not — a node upgraded onto a machine without podman keeps
  running as before. An explicit `on` without a runtime makes `mesh
  node start` refuse to start (fail closed; the service manager retries
  it), rather than heartbeating `harness_unavailable` every tick; that
  per-tick re-check is later, with the image pull.
- **Network enforcement** is pasta with `--no-map-gw` and
  `--no-hosts`, DNS through `169.254.1.1` (pasta's `--dns-forward`),
  and an nftables `inet` table loaded by a `--cap-add NET_ADMIN` init
  container from the node image (`nft -f -`): accept `lo`, established
  replies and DNS to the forwarder; drop the LAN, loopback, link-local,
  CGNAT, IPv6 loopback/ULA/link-local and the host's public addresses;
  accept TCP 443 and 80; drop the rest. A pod whose rules do not load
  is removed and the session errors.
- **Limit kills.** `--memory` with the same `--memory-swap`, `--cpus`,
  `--pids-limit`; the runtime's `--timeout` is the session timeout plus
  30 s, a backstop behind the node's own context (whose timeout is
  handled as before). An exit of 137 that podman reports `OOMKilled`,
  or that the node did not cause, is `memory`; 137 at or past the
  timeout is `timeout`; a failing exit whose stderr says fork could not
  get a process is `pids`. Each is a `*sandbox.LimitError`: the runner
  releases the item with the reason, records a decline so the node does
  not retake it for `DeclineHold`, and `Loop.NoteCondition` sends the
  reason as the heartbeat's `reason` for an hour while the node stays
  online.
- **The node image** (`images/node/Containerfile`, embedded in the
  binary as `nodeimage.Containerfile`; `internal/sandbox/image.go`).
  Debian with git, Go at go.mod's version (a test holds them together),
  Node LTS with npm and corepack, Python 3 with pip and uv, make and a C
  toolchain, gh, nftables, ca-certificates, a Postgres server
  (`postgresql-17`, `initdb`/`pg_ctl`/`psql` in `/usr/local/bin`, no cluster made)
  and headless Chromium (Debian's `chromium`) — amended 2026-10-13,
  below — and the claude and codex CLIs, each at a version pinned in
  the file; a non-root user `mesh`;
  no secret. Neither agent updates itself: the CLIs are root-owned
  under `/usr/local`, `DISABLE_AUTOUPDATER=1` is in the image's ENV and
  — since `--unsetenv-all` drops that — in the sandbox's own `BaseEnv`,
  and `/etc/codex/config.toml` turns codex's update check off.
  `release-tag.yml` pushes it per tag (mesh-cli ADR-002 amendment).
- **Pull before the drain.** A self-update (`Loop.Image`) and `mesh
  node update` (`nodeUpdate.Image`) call `SandboxConfig.EnsureImage`
  for the *new* version once its release is verified and before they
  drain: `sandbox.EnsureImage(ctx, runner, ref, keep…)` pulls the ref
  and removes every other `ghcr.io/runsheet/mesh-node` tag but the
  new one and the running version's (its sessions still use it until
  they end; the next update removes it). Since 2026-10-13 a
  self-update swaps without a drain, sandboxed or not (below); the
  pull still comes first. The runner is
  injected (`sandbox.Runner`; `ExecRunner` in the node). A failed pull
  is returned: the self-update refuses the version with it as
  `updateError`, the command exits non-zero, nothing is drained or
  swapped, and the node stays on its old binary. Nothing falls back to
  a local build on its own. `sandbox: off` asks nothing; a mode left
  out on a machine whose `podman --version` fails is skipped the same
  way; an explicit `on` without podman is the error. The pull does not
  yet verify the cosign signature.
- **`sandbox.image`**: empty is `sandbox.DefaultImage(version)`,
  `ghcr.io/runsheet/mesh-node:<version>` (`:dev` for a dev
  build); `build` is that same ref built here by `sandbox.BuildImage`
  from the embedded Containerfile (`--build-arg MESH_VERSION=<version>`)
  in place of the pull, and the sessions run it; any other value is
  used as given, pulled the same way. `mesh node update <running
  version>` ensures the running version's image, which is how a node
  set to `build` makes its first one.
- **Heartbeat.** `HeartbeatExtras.Sandbox` → `sandbox {mode, runtime,
  image}` and `HeartbeatExtras.Reason` → `reason`, each only when set;
  the image is the reference, not yet its digest.
- **Services: the node opens the pod before the agent.** Build,
  evaluate and rebase sessions call `OpenSessionPod`: it opens the
  sandbox session itself, `Resolve`s the checkout and `Session.Start`s
  the pod with the services in it, so the node — not the harness —
  decides from the outcome whether the agent starts, and puts the
  result in the prompt. The session goes to the harness on
  `Job.Session`; `Run` runs the agent in the pod `Start` made, and the
  node's deferred `Close` removes the pod, its sidecars and the HOME on
  every way out (Run's own removal covers the agent's end). Plan and
  research sessions start no services.
- **Sidecars.** `podman run -d --pod … --pull missing` (the node user's
  store is the image cache; pruning is later), nothing mounted, env by
  name, `--name <pod>-<service>`. The services budget
  (`services_cpus`, `services_memory`) is split evenly between the
  sidecars, each also capped at 1024 processes. Known databases' data
  stays in the container's own layer (no tmpfs yet); it goes with the
  pod. A sidecar listens on its declared *container* port: nothing is
  published on the host.
- **Health checks.** The declared `--health-cmd` runs in the sidecar
  (`podman exec <c> sh -c …`, a leading `CMD-SHELL` dropped); with none,
  a TCP connect to the first port from a short-lived node-image
  container in the pod (bash's `/dev/tcp`); with no port, a started
  service is ready. The spec's per-database default commands are not
  implemented: the TCP connect stands in for them. Each check runs
  every `--health-interval` (default 1 s, not Docker's 30 s), times out
  at `--health-timeout` (5 s), fails after `--health-retries` failures
  past `--health-start-period`, and all are awaited together within
  node.yaml's `sandbox.service_timeout` (default 120 s).
- **Outcomes.** All ready: the agent starts. The prompts (build,
  evaluate, rebase) get a `# Services` section before their rules, and
  `$HOME/.mesh-services` lists each service tab-separated — name,
  image, `127.0.0.1:<port>`s, `ready` or `failed: <why>`. An optional
  service that fails or is refused is a progress note naming it and
  why, and the agent is told it is missing. An item in `evaluating`
  takes no progress (the platform refuses it), so an evaluation puts
  that note in its verdict's notes. A required one that fails or is
  refused starts no agent: a build is released (`ReleaseWorkItem`,
  `services: required service <name> (<image>) did not come up:
  <why>`) and held off in `Declines`; an evaluation is left unevaluated
  and held off the same way; a rebase is failed with that reason. A
  required service refused for its registry makes no pod at all, so
  nothing is pulled. A `.mesh/services.yml` that cannot be read is a
  required service that failed.
- **Git identity.** The session's `.gitconfig` carries the session's
  agent identity, never the operator's (amended 2026-10-03, below).

### Amendment 2026-09-28: a build's validation runs in the session's pod

A build's declared validation ([ADR-014](adr-014-a-build-validates-before-it-pushes.md))
runs each step as one `Session.Run` of the build's own session — a
fresh pod of it, since the agent's went with the agent — with the
workspace mounted and the same allow-listed env; the attestation names
the node image by digest (`sandbox.ImageDigest`) and says `sandboxed:
true`. A limit kill there is the step's failure, not the node's
condition. With `sandbox: off` the steps run on the host and the
attestation says `host:<hostname>`.

## Consequences

- A sandboxed node needs accounts: the operator's own `~/.claude` login
  is not mounted, so a session with no account the platform or
  node.yaml gives it cannot authenticate.
- The node image must carry `nft` and put its toolchains on the fixed
  `PATH` the sandbox sets.
- The pids heuristic reads stderr; a session that swallows fork errors
  fails as an ordinary exit.

## Amendment — 2026-10-02 (goal builtin-is-mesh-context)

The built-in attachment is `mesh-context` (`tools.Builtin`, mesh-cli
#264/#396); `prologue` is the deprecated alias read until 2026-10-28.
"Where it plugs in" said the harness writes the MCP config to
`$HOME/mcp.json` "the Prologue bearer on no command line" — the
built-in's old name, left unfixed in this decision after the rename
reached the node's own attachment (`adr-006`'s 2026-09-28 amendment).
It now says the mesh-context bearer; nothing about where the config is
written or how it is read changes.

## Amendment — 2026-10-03 (goal every-actor-has-a-name, item 2)

"Git identity" said the session's `.gitconfig` carries the operator's
`user.name`/`user.email`, read from the host's global git config at
start. It no longer does: no session commits as the node's owner. The
`.gitconfig` names the session's agent, `Mesh <role> (<slug>)
<mesh-<role>+<slug>@runsheet.dev>`, passed to `Sandbox.Open` per session
(`sandbox.GitIdentity`); `sandbox.Config.GitName`/`GitEmail` and
`nodecmd.go`'s `gitIdentity` are gone. The checkout's hooks directory
(`<workdir>.hooks`, its `prepare-commit-msg` adding the paired person's
`Co-Authored-By`) is mounted read-only beside the workspace, and the
`GIT_AUTHOR_*`/`GIT_COMMITTER_*` variables are allowed into the pod.
An evaluation's pod gets no identity. [ADR-024](adr-024-a-session-commits-as-its-agent.md)
is the decision.

## Amendment — 2026-10-10 (goal the-agent-container-owns-its-home)

The pod maps the host user onto the image's user:
`--userns keep-id:uid=<image uid>,gid=<image gid>`, where it used to
pass a bare `--userns keep-id`. `Podman.CreatePod` reads the image's
declared user with `sandbox.ImageUser` (`podman image inspect --format
{{.Config.User}} <image>`) on the Podman's own run path, fills
`PodSpec.UID`/`GID`, and `PodCreateArgs` formats the flag with
`KeepIDUserns`. An image that declares no user, or one by name only,
falls back to `DefaultImageUID`, 1000 — the node image declares `USER
1000:1000` (images/node/Containerfile) — and an inspect that fails
uses 1000 too and makes the pod anyway: a missing image is the agent
run's error to report, not the mapping's.

Why: on the dl360 (2026-10-10) the nodes run as Linux users uid 1002
and 1003. A bare `keep-id` maps the host user onto the same uid inside
— 1002 — while the agent runs as the image's 1000, and the session
HOME the node makes 0700 with 0600 files was unreadable: every
sandboxed session died at its first read, `Invalid MCP configuration:
EACCES: permission denied, open '/tmp/mesh-home-…/mcp.json'`. It only
ever worked for a uid-1000 operator. Mapped onto the image's user, the
HOME, the workspace and the context files the node writes as the host
user are the agent's own inside.

keep-id's `uid=` and `gid=` options need **podman 4.3 or later**; that
is the sandbox's floor. An older podman refuses the flag at `pod
create`, and the session errors as a pod that cannot be made does.

## Amendment — 2026-10-10 (goal a-sessions-temporary-home-is-removed-when-its-pod-ends)

On the dl360 (2026-10-10) sandboxed sessions left 370 `/tmp/mesh-home-*`
directories, 150 GB, in a day — 14 in use — and exited containers and
pods with nothing running. "One package, one interface" said
`Session.Close` removes anything left and the HOME; it tried, with a bare
`os.RemoveAll` whose error was dropped, and what the agent writes into
its HOME — Go's 0555/0444 module cache, files keep-id's mapped subuids
own — made it fail. A node killed or restarted left the rest: a
sandboxed session wrote no session state then, so no later process
could find it to adopt (since 2026-10-13 it does: the pod is the
session's unit, below).

- **Close is sure, and says when it is not.** `Session.Close` removes
  every pod the session made with `Runtime.Remove` — `Podman.Remove` runs
  `RemoveArgs`: `podman rm --force --volumes --time 5 --filter
  pod=<pod> --filter name=^<pod>-` (the agent's and the sidecars' own
  containers with their anonymous volumes), then `podman pod rm --force
  --time 5 <pod>` — and the HOME with `removeHome`: `os.RemoveAll`;
  failing that the tree made owner-writable and `RemoveAll` again;
  failing that the runtime's new `RemoveTree`, `podman unshare rm -rf
  <path>`, inside the node user's user namespace where the subuid-owned
  files are its own, then the path checked gone. Close returns every
  failure joined, and the node never drops it: `SessionPod.Close`, the
  harness's own session (`Session.CloseAndLog`) and a validation's own
  session log `sandbox: session <label>: close: <err>` (`CloseLine`)
  through the node's logger (`OpenSandbox` sets `Sandbox.Logf`).
- **Every way a session ends tears it down**, each proved by a test with
  the `Fake` and a sandbox whose `TempDir` is the test's
  (`internal/node/sandbox_teardown_test.go`: no `mesh-home-*` left, every
  pod made removed): a build that finishes, an evaluation that finishes,
  a hand-back (a required service down, released before any agent), a
  limit kill, a drain (the node's context cancelled while the agent
  runs), the harness's own session, and a validation that opens its own.
- **A node start sweeps what an earlier process left.**
  `sandbox.Sweep(ctx, rt, tempDir) (SweepResult, error)`
  (`internal/sandbox/sweep.go`) reads the store with the Runtime's new
  `ListContainers` (`podman ps --all --format json`, a running one's
  host mounts from `podman container inspect`) and `ListPods` (`podman
  pod ps --format json`) and removes, in order: every exited container of
  a session — labelled `mesh.session`, named `mesh-…`, or in a mesh pod —
  by `RemoveContainer` (`podman rm --force --volumes`), so its anonymous
  volumes go; every mesh pod (labelled `mesh.session` or named `mesh-…`)
  with no running container but its infra, by `Remove`; and every
  `mesh-home-*` directory directly under the sandbox's `TempDir`, owned
  by the node's user, that no running container mounts (the directory
  or a path inside it), by `removeHome`. A HOME a running container
  mounts — a session still running — is counted in use and never
  touched; another node user's HOME in a shared `/tmp` is that node's
  to sweep against its own store; a listing that fails sweeps nothing,
  since without it a HOME in use cannot be told from a stale one.
  `node.SweepSandbox` runs it in `mesh node start`, poll and channel
  mode alike, after the sessions of the process before are adopted and
  before the first tick, and logs `sandbox: swept N homes, M pods, K
  containers at start (L homes in use)` (`SweepResult.Line`), a failed
  removal as `sandbox: sweep at start: <err>`. `sandbox: off` sweeps
  nothing.

The hourly cron the dl360 ran in the meantime (`/etc/cron.hourly/mesh-home-sweep`)
has the same shape and goes once this lands there.

## Amendment 2026-10-13: the image carries Postgres and headless Chromium

Goal `a-node-validates-every-enrolled-repo-with-the-tools-it-needs`,
item 1. latchkey's `conformance` step (`scripts/time-conformance.sh`)
needs headless Chrome and a Postgres server; the image had neither, so
it exited 1 on untouched main and every item failed on its environment.

- **In the image.** `postgresql-${PG_MAJOR}` and
  `postgresql-client-${PG_MAJOR}` (`PG_MAJOR=17`, trixie's) with
  `/usr/lib/postgresql/${PG_MAJOR}/bin`'s binaries linked into
  `/usr/local/bin` — on the session's PATH, which is
  `sandbox.DefaultPath` and not the image's ENV (`--unsetenv-all`),
  ahead of `/usr/bin`'s pg_wrapper — so `initdb`, `pg_ctl`, `postgres`
  and `psql` answer to the session's non-root user; `create_main_cluster = false` before the install, so
  the image holds no cluster and no running server — a check runs
  `initdb` into a temp dir of its own and starts it on a unix socket
  there. And `chromium` (with `fonts-liberation`), `/usr/bin/chromium`.
- **Why Debian's Chromium, not Chrome for Testing.** The image is
  built for linux/amd64 and linux/arm64 (`PushImage`); Debian trixie
  builds `chromium` for both, from the same security-updated source,
  while Chrome for Testing ships no linux arm64 build. A check that
  looks for `google-chrome` or `chrome` finds neither; one that takes
  `CHROME_PATH`/`PUPPETEER_EXECUTABLE_PATH` is given
  `/usr/bin/chromium` by the repo. In a pod Chromium runs with
  `--no-sandbox`: the pod is the sandbox, and a rootless pod has no
  user namespace left for Chromium's own.
- **In the image, not in a repo's `validate.yml`.** A browser and a
  database server are a machine's packages (apt, root, a few hundred
  MB), which a session's non-root user cannot install and a
  `validate.yml` step would download on every run. What a repo can
  install as its user — its npm packages, a Go module — stays the
  repo's, in its lockfile.
- **Proven in the image.** `scripts/image-tools-check.sh` prints one
  PASS/FAIL line for node/npm with `npm ci && npx tsc --version` in
  `images/node/testdata/tools-fixture`, for a Postgres server started
  and `select 1`, and for Chromium's `--headless --version` and a
  headless `--dump-dom`; `TestImageToolsE2E` (`-tags e2e`,
  `internal/sandbox/tools_e2e_test.go`) builds the embedded
  Containerfile, or takes `MESH_TOOLS_IMAGE`, and runs it there
  (docs/e2e-sandbox.md, Tools check). The image's own build checks
  `initdb`/`pg_ctl`/`psql --version` and `chromium` by its package, as
  it does nftables: the arm64 build runs under qemu-user.

## Amendment 2026-10-13: the image is pulled on a registry credential from the broker

`ghcr.io/runsheet/mesh-node` refuses anonymous pulls (mesh-cli ADR-002's
amendment, "The package is private while the repository is"), and the
node pulled it with a bare `podman pull <ref>`: on goal budget-pause the
pull of `ghcr.io/runsheet/mesh-node:dev` was `unauthorized`, so a
sandboxed node could not ensure its image unless its operator had run
`podman login ghcr.io` by hand. The pull is now authenticated the way
`PushImage`'s push already is:

- **The credential comes from the broker, the platform.**
  `sandbox.EnsureImage(ctx, run, ref, broker, keep...)` asks its
  `RegistryBroker` for `RegistryHost(ref)` — the ref's first component
  when it names a host, else `docker.io`. On the node the broker is
  `node.RegistryBrokerOf(gateway.Client)`, which reads
  `nodeRegistryToken(registry: String!): Map` →
  `{registry, username, token, expiresAt}`
  (`gateway.Client.NodeRegistryToken`): for `ghcr.io`, a GitHub App
  installation token with `packages: read` on the node image's package,
  issued to the node the bearer names. No item is held when an update
  ensures its image, so the item's `issueItemRepoToken` (contents on one
  repo) is not the source; nor is any token the node already has —
  neither the gateway's schema nor `specs/contracts` carried a registry
  credential before this. `SandboxConfig.EnsureImage` takes the broker;
  `ensureNodeImage` (`mesh node update`, `mesh node upgrade`, the
  self-update's `Loop.Image`) passes `RegistryBrokerOf` on
  `platformClient()`.
- **On an auth file of its own.** The credential is written as a
  containers `auth.json` logging in to that registry (`registryAuthAs`;
  user `x-access-token` when the broker names none) in a fresh temporary
  directory, passed by `podman pull --authfile <file> <ref>`, and the
  directory removed once the pull returns, whether it pulled or failed.
  It is never in the image, a build context, the operator's
  `~/.config/containers/auth.json` or a session's HOME.
- **An image that needs no credential still pulls.** A broker with none
  for the registry (a null answer), a platform without
  `nodeRegistryToken` (`IsUnsupported`), a nil broker (no
  `~/.mesh/token` to ask on) — each pulls with no `--authfile`, on
  podman's own login, as before. A broker that errs is not the pull's
  failure: the pull goes as podman's login has it, and if that fails its
  error ends `(registry credential: <the broker's error>)`.
- **A session's pod does not pull the node image on a credential.** The
  agent and validation containers run with `--pull missing`
  (`internal/sandbox/podman.go`), so they use the image the update
  ensured; a node whose image is missing at session start still pulls it
  anonymously there and fails as before. Service sidecars pull only
  public images (mesh-specs ADR 007, Services), anonymously.
- **The platform half.** `nodeRegistryToken` is a platform field this
  change reads before the platform serves it; until it does every pull
  goes as before, so the rollout is not a flag day. The fake gateway
  answers it from `RegistryTokens` (`gatewaytest`).
  `TestEnsureImagePullsOnTheBrokersCredential` (`internal/sandbox`) and
  `TestEnsureImagePullsOnTheGatewaysRegistryToken` (`internal/node`)
  prove the `--authfile`, its login and its removal after a pull that
  succeeds and one that fails.

## Amendment 2026-10-13: the pod is the session's unit (goal a-sandboxed-session-outlives-the-node)

A sandboxed session ran its agent attached — `podman run -i`, the node
holding its stdin and stdout — and wrote no session state, so it ended
with its node and no later process could adopt it; a sandboxed node's
self-update drained first, waiting out its longest build. mesh-specs
ADR 008 makes a session's unit, not the node, its owner; for a
sandboxed session that unit is now its pod (mesh-cli ADR-009's
amendment of the same date, Sandboxed sessions):

- **The agent runs detached.** A sandboxed `Job.Outlive` session
  (`ClaudeCode.DetachSandboxed`, set by `mesh node start` with a
  sandbox) starts its agent with `Session.StartDetached` →
  `Runtime.StartAgent` — `podman run -d` (`StartAgentArgs`) in the
  session's pod, beside its sidecars, under the same limits, mounts,
  env and `--userns keep-id` as an attached run (the dl360's podman shim
  rewrites that flag and is unaffected) — its stdin, stdout and stderr
  redirected inside the container to `<workdir>.session/.prompt`,
  `output.jsonl` and `stderr.log`, each file's directory mounted at its
  own path. The container is named `sandbox.AgentName(pod)`
  (`<pod>-agent`); `AgentState` (`podman container inspect`),
  `StopAgent` (`podman stop --time 10`) and `WaitAgent` find, read and
  stop it by that name from any process.
- **`state.json`.** The session writes the host session's state file
  with `runner: podman` (`sessionunit.SandboxRunner`), `pid` 0, `unit`
  and `container` the agent's container, and `pod` and `home`: what a
  node after this one needs, and never a secret. The prompt file is
  removed after the agent's first event.
- **Letting go.** The node's cancel `Detach`es the session: `Close`
  after it removes nothing, so the pod, its sidecars and the HOME stand
  while the agent works. Only the session's own timeout stops it
  (`--timeout` on the container, and the attacher at `startedAt +
  timeout`).
- **Adoption and the sweep.** `FindSessionsIn(root, ids, sb)` finds it
  and judges it by its container; `Runner.AdoptItem` reattaches the pod
  and HOME (`Sandbox.Reattach`, or `ReattachHome` when the pod is gone)
  so validation runs in them, then removes both after the finish. A
  session that is no longer the node's is stopped and its pod, HOME and
  session directory removed (`harness.RemoveSandboxedSession`). The
  start-up sweep above runs after adoption and keeps every found
  session's pod, containers and HOME, running or exited
  (`sandbox.SweepKeeping`); the HOME rule — a `mesh-home-*` no running
  container mounts is swept — still holds for everything else.
- **The self-update does not drain.** With the pod outliving the node,
  `Loop.update` swaps at once on a sandboxed node too (ADR-009's
  amendment), logging the no-drain line with ` (sandbox: on)`.
  `TestSandboxedSelfUpdateMidSessionFinishesOnTheNewNode` proves an
  upgrade mid-session on `sandbox.Fake`: the agent runs on, the new
  loop adopts and finishes it, the item's heartbeats never stall.
- An attached run (`Session.Run`) remains for every session that is not
  `Outlive` — validation steps, channel mode — and those still end with
  the node.

## Amendment 2026-10-16: a postgres service gets a writable PGDATA

Goal `the-sandbox-gives-postgres-a-data-directory-it-can-write`. Under
keep-id (the 2026-10-10 amendment above) the image's
`/var/lib/postgresql/data` belongs to the pod's user, not the uid
postgres's entrypoint runs as inside, and the entrypoint's `chmod` of it
fails: `initdb: could not change permissions of directory
"/var/lib/postgresql/data": Operation not permitted`. Every evaluation
of foghorn, purser and loom's "pull requests are judged by the node"
items ended not evaluated this way on 2026-10-16.

`internal/sandbox/services.go`'s `finish` now gives every resolved
`postgres` or `postgis` service, from either source —
`.mesh/services.yml` or a workflow's `services:` — `PGDATA=/tmp/pgdata`
and `POSTGRES_INITDB_ARGS=--no-sync` (`postgresDefaults`,
`PostgresDefaultPGDATA`, `PostgresDefaultInitdbArgs`) unless the
repository already set that variable itself, in which case its own
value is kept. `/tmp` is writable by the pod's user under keep-id, and
`--no-sync` keeps the default's `initdb` as cheap as the tuning already
turns the running server's durability off.
