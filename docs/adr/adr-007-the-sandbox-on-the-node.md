---
summary: How mesh-cli implements mesh-specs ADR 007 — internal/sandbox's Runtime (Podman, a test Fake) runs claude in a per-session pod with two mounts, an allow-listed env passed to podman by name, node.yaml's flat sandbox keys (mode, cpus, memory, pids, services_cpus, services_memory, registries, image), a mode left out following `podman --version` while an explicit on fails closed at start, a limit kill (OOM, pids, the runtime's timeout backstop) released as the node's condition with the heartbeat's reason, service sidecars started and health-checked in the session's pod before the agent (a required one failing releases the claim, an optional one is a note), the node image (images/node/Containerfile, embedded) pulled for the new version before a self-update or `mesh node update` drains — a failed pull an updateError with no drain and no swap, `sandbox.image: build` building it locally — and channel mode and Codex not yet sandboxed.
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
  `$HOME/mcp.json` (the Prologue bearer on no command line), and runs
  `claude` with the same arguments otherwise in the pod. Every one-shot
  session (poll-mode build, evaluate, plan, research, rebase) goes
  through it. Channel-mode sessions and Codex are later items of the
  goal.
- **Env by name.** The container's variables are given to `podman run`
  as `--env NAME` with the values in podman's own process environment,
  and `--unsetenv-all` drops the image's and podman's defaults, so no
  secret is on a command line and nothing else reaches the agent.
  Podman's process keeps only what it needs to find its storage
  (`PATH`, `HOME`, `XDG_RUNTIME_DIR`, …); the container's `HOME` wins.
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
  toolchain, gh, nftables, ca-certificates and the claude and codex
  CLIs, each at a version pinned in the file; a non-root user `mesh`;
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
  the drain ends them; the next update removes it). The runner is
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
- **Git identity.** The session's `.gitconfig` carries the operator's
  `user.name`/`user.email`, read from the host's global git config at
  start, since the session no longer reads the host's.

## Consequences

- A sandboxed node needs accounts: the operator's own `~/.claude` login
  is not mounted, so a session with no account the platform or
  node.yaml gives it cannot authenticate.
- The node image must carry `nft` and put its toolchains on the fixed
  `PATH` the sandbox sets.
- The pids heuristic reads stderr; a session that swallows fork errors
  fails as an ordinary exit.
