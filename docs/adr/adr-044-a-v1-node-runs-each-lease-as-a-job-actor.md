---
summary: A Mesh v1 node (internal/nodev1) runs each lease as a job actor that owns everything the lease uses and leaves only through releasing; a scheduler alone owns the node's capacity as leases with a TTL and reports `free` on every take; a supervisor watches progress and reconciles the node every minute; resource actors trim the build cache and self-update by draining. Each piece lands behind a node flag, off by default.
status: proposed
---

# ADR-044: A v1 node runs each lease as a job actor

2026-10-30. Mesh v1 wave 3 (the actor node). Applies to `mesh node run
--api v1` (internal/nodev1) only; the legacy runtime (internal/node) is
unchanged.

## Context

The legacy node keeps what is in flight in maps beside its loop — claims,
pods, CPU reservations, relay grants — and every path that ends a job has
to remember to clean each of them up. When one is missed the node leaks:
pods with only a sidecar left (2026-10-17's zombie children), a CPU
budget that never comes back, a cache trim that runs under a live
validation (2026-10-28, `go clean -cache` failing every Go step), an exit
137 from a pod someone else killed counted as the item's red. Wave 0's
nodev1 runs one lease at a time and has the same shape in miniature.

## Decision

1. **A job actor per lease.** It owns the lease, its job directory
   (`leases/<lease_id>`), its pods, its scheduler lease (CPUs, memory, a
   job slot, an account slot, the build cache shared), its relay grant
   and its renewal. Its states are taken → preparing → running →
   reporting → releasing → done, and cancelling from any state.
   `actor.json` in the job directory is written before each state
   change. Releasing is the only way out — success, failure, cancel,
   timeout and panic all pass through it; a panic is recovered into
   releasing — and it closes what the actor holds in reverse order.
2. **Ends.** Every end maps to a finish, a work failure, or a hand-back
   with a cause: `cannot_run`, `pod_killed`, `stalled`, `node_update`,
   `node_restart`, `panic`, `revoked`, `parked`. An exit 143 or 137 the
   actor did not cause is `pod_killed`, never red. A node that restarts
   reads every `actor.json` left behind and hands those leases back
   `node_restart`, then releases what they held.
3. **The scheduler alone owns capacity** (internal/scheduler). Capacity
   is held as leases with a TTL: CPUs, memory, job slots, account slots,
   and the Go build cache shared or exclusive. A holder that stops
   renewing gives its capacity back when its TTL passes. What no lease
   holds is the `free` the node reports on every take, and the node
   takes only while a job slot is free.
4. **A supervisor** (internal/supervisor) watches each actor's progress
   (the streaming session's `progress_at`): quiet for `quiet_after`, it
   escalates first through the lease's mailbox, then — in `cancel` mode
   — stops the actor with cause `stalled` (`warn` only logs). Every
   minute it reconciles the node: pods with no actor and pods running
   only a sidecar are killed, orphaned job directories and relay grants
   are removed, adopted sessions are counted.
5. **Resource actors** (internal/resources). The cache trim runs off the
   take loop holding the build cache exclusively, waiting at most
   `trim_wait` for builds to drain. Self-update drains the node, then
   hands back what still runs with cause `node_update`.
6. **No in-flight maps.** Once every piece is on, nothing in nodev1
   holds a lease's state outside its actor; a guard test holds it so.

## Rollout

Each piece is a PR behind a `mesh node run` flag, off by default:

| PR | Flag |
|---|---|
| 3.1 scheduler and the leases ledger | `--actors-scheduler` (with `--account-slots`) |
| 3.2 job actor, `actor.json`, panic recovery, `pod_killed` | `--actors` |
| 3.3 supervisor liveness and reconcile | `--supervisor=off\|warn\|cancel` |
| 3.4 resource actors | `--cache-trim`, `--self-update` |
| 3.9 guard test | none |
| the lease's pod (amendment below) | `--sandbox` (with `--actors`) |

## Amendment, 2026-10-30: the lease's pod (`--sandbox`)

Until now nodev1 made a pod only in tests: a job actor held one for the
brief's pods, and the session ran on the host. With `--sandbox` each
lease's session and its services run in one rootless podman pod of the
lease's own, on the legacy node's sandbox (mesh-specs ADR 007) and
node.yaml's `sandbox:` settings (image, cpus, memory, pids, the
services' budget, registries, service timeout, tmpfs).

1. **One pod per lease, the actor's.** It is named `mesh-v1-<lease>`
   before it exists, so actor.json names it (a restarted node removes
   it), and labelled `mesh.v1.lease`, `mesh.job` and `mesh.node`, which
   the supervisor's reconcile reads. Its agent container is
   `<pod>-agent`, the name watchPod and the reconcile watch.
2. **Services.** The checkout's `.mesh/services.yml` (or a workflow's
   services, as the legacy node reads them) and the brief's pods start
   as sidecars, health-checked before the session. A required one that
   is not ready hands the lease back `cannot_run`. `run_suite` runs its
   steps in the same pod, beside them.
3. **Resources.** The agent runs on the CPUs and memory the scheduler
   leased the lease (the brief's `resources`); a zero keeps node.yaml's.
4. **The relay through the socket only.** The pod mounts the lease's
   socket directory and nothing else of the relay: product MCP through
   the node image's proxy shim, vendor APIs at `/latchkey/<vendor>/` on
   `$MESH_RELAY_SOCKET`, and git through a loopback bridge in the agent's
   container (`node mesh-git-bridge.js <socket> <port> -- claude …`)
   that the checkout's origin is rewritten onto by `GIT_CONFIG_*`
   `insteadOf`, since git cannot speak to a unix socket. The model relay
   (#1691) is reached through the same bridge: the pod's
   `ANTHROPIC_BASE_URL` is `http://127.0.0.1:<bridge>/model/anthropic`
   (`OPENAI_BASE_URL` `…/model/openai/v1`), with the relay's dummy
   credential and `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` as a host
   session has them; the bridge runs whenever the lease has a git grant
   or relays a model account. The pod's
   environment is the sandbox's allow-list: none of the node's own.
5. **Ends.** An agent exit 137 or 143 the actor did not cause, or a
   sandbox limit's kill (memory, pids), is `pod_killed`.
6. **Never root.** `--sandbox` refuses to start as root, and with no
   podman that answers: a lease never runs unsandboxed because the
   runtime is missing.

The real-pod path is validated on dl360 (podman 5, rootless); the
laptop's tests drive it through `sandbox.Fake`, with the agent's command
— the git bridge and the fake claude — run on the host.
