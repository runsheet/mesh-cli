---
summary: How mesh-cli implements mesh-specs ADR 007 — internal/sandbox's Runtime (Podman, a test Fake) runs claude in a per-session pod with two mounts, an allow-listed env passed to podman by name, node.yaml's flat sandbox keys (mode, cpus, memory, pids, services_cpus, services_memory, registries, image), a mode left out following `podman --version` while an explicit on fails closed at start, a limit kill (OOM, pids, the runtime's timeout backstop) released as the node's condition with the heartbeat's reason, and channel mode, Codex and services not yet sandboxed.
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
  (`Version`, `CreatePod`, `RunAgent`, `AddSidecar`, `Remove`), a
  `Podman` that shells out to `podman` one call per step, and a `Fake`
  that records the specs for tests. `Sandbox.Open` makes the session's
  HOME, `Session.Run` creates the pod, runs the agent and removes the
  pod, `Session.Close` removes anything left and the HOME.
- **Where it plugs in.** `harness.ClaudeCode.Sandbox`: nil is the old
  exec path, untouched; set, the harness writes the MCP config to
  `$HOME/mcp.json` (the Prologue bearer on no command line), and runs
  `claude` with the same arguments otherwise in the pod. Every one-shot
  session (poll-mode build, evaluate, plan, research, rebase) goes
  through it. Channel-mode sessions, Codex and service sidecars are
  later items of the goal.
- **Env by name.** The container's variables are given to `podman run`
  as `--env NAME` with the values in podman's own process environment,
  and `--unsetenv-all` drops the image's and podman's defaults, so no
  secret is on a command line and nothing else reaches the agent.
  Podman's process keeps only what it needs to find its storage
  (`PATH`, `HOME`, `XDG_RUNTIME_DIR`, …); the container's `HOME` wins.
- **node.yaml keys are flat**: `sandbox.{mode, cpus, memory, pids,
  services_cpus, services_memory, registries, image}` rather than the
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
- **Heartbeat.** `HeartbeatExtras.Sandbox` → `sandbox {mode, runtime,
  image}` and `HeartbeatExtras.Reason` → `reason`, each only when set;
  the image is the reference, not yet its digest.
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
