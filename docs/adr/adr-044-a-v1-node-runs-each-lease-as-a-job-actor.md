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
   or relays a model account. The Stop hook of finish enforcement
   (#1692) runs in the pod as the node image's `node` on a shim in the
   session's HOME (`mesh-stop-hook.js`), asking the gate on its socket
   in the mounted socket directory; the pod has no mesh binary. The pod's
   environment is the sandbox's allow-list: none of the node's own.
5. **Ends.** An agent exit 137 or 143 the actor did not cause, or a
   sandbox limit's kill (memory, pids), is `pod_killed`.
6. **Never root.** `--sandbox` refuses to start as root, and with no
   podman that answers: a lease never runs unsandboxed because the
   runtime is missing.

The real-pod path is validated on dl360 (podman 5, rootless); the
laptop's tests drive it through `sandbox.Fake`, with the agent's command
— the git bridge and the fake claude — run on the host.

## Amendment, 2026-11-02: no slot limit (`--slots 0`)

A v1 node may be scheduled on CPUs and memory alone. Until now
`mesh node run --slots N` registered `max(N, 1)`, so `--slots 0` meant
one session at a time.

1. **`--slots 0` is no slot limit.** The node registers
   `capacity_total.slots` 0 (omitted on the wire), which Mesh's take
   already reads as no slot limit (projectmesh-io/api `queue/fit.go`):
   a lease fits on CPUs and memory.
2. **The scheduler agrees.** With `--actors-scheduler`, a total of
   Slots 0 holds no job slot against any lease: the node takes again at
   once, a lease is admitted while its brief's CPUs and memory fit
   beside the live leases, and the take's `free` carries no slots (a
   hint of 0 says nothing of slots to Mesh). `--account-slots` still
   bounds sessions per model account.
3. **Without the scheduler** the node still runs one lease at a time and
   says one slot free on each take, whatever its total.
4. **The default stays 1.** A node not passing `--slots` behaves as
   before; any N above 0 is reported and enforced as given. The v0 node
   (`mesh node start`) has no `--slots` and is unchanged.

A node with no slot limit should say its memory (`--mem-gb`): with
memory 0 Mesh fits only jobs that ask none, as before.

## Amendment, 2026-11-02: the operator's drain (`mesh node drain`)

A v1 node had no way to restart that let its leases finish. Its service
manager's restart is a SIGTERM, which ends Run's context: every lease is
handed back `node_restart` and its job starts again from the beginning
on the next take. On 2026-11-02 every deploy of dl360's v1 node did
exactly that. The legacy node had `mesh node drain` (SIGUSR1 through
`~/.mesh/node.pid`); the only drain in nodev1 was the one the
self-update and Mesh ask for.

1. **A pidfile of its own.** `mesh node run --api v1` writes its pid to
   `~/.mesh/node-v1/node.pid`. A legacy node and a v1 node run by one
   user keep apart: their state is in different directories. Beside the
   pid are:
   - `node.draining`, the draining marker;
   - `node.status`, the node's status (registration, drain, live
     leases). It is written at each heartbeat, as a lease starts or
     ends, and as the node drains.

   All three are removed as the process exits.
2. **SIGUSR1 drains.** `mesh node drain` signals the running node.
   - With a legacy and a v1 node both running as the user, `--api v0|v1`
     says which; otherwise it is the one running.
   - The node takes no new lease. A wait for a free slot ends at the
     drain. A take already in its long poll is not cut off: a lease Mesh
     placed but the node never heard would sit unrun until its TTL
     lapsed. Mesh, told of the drain, answers that take 204 on its next
     pass (within `MaxWaitS`, 25 s). A lease it had already placed runs,
     and the drain waits for it.
   - The node asks Mesh to stop offering it work with `POST
     /v1/nodes/{id}/drain {drain: true, reason}`. The take answers 204,
     and Mesh's health reads show the node as draining.
   - The running leases finish as they would. Once none runs, Run ends
     nil (the Mesh call answered first), the process exits 0, and
     systemd (`Restart=always`) starts the binary now on disk.
3. **`--wait <dur>`.** When the wait is up, what still runs is handed
   back `node_update`, as the self-update's drain wait does: the job
   keeps its place and no attempt is spent. The node then exits as
   above. The wait reaches the node in `node.drain-wait`, written before
   the signal and read once as it arrives. A second drain may shorten
   the wait, never lengthen it.
4. **The drain is resumed by the node that asked for it.** Mesh keeps a
   drain across a restart; only `drain: false` clears it. Before the
   node calls Mesh, it marks the work root with `mesh-drained`. Its next
   start finds the mark and calls `POST /v1/nodes/{id}/drain {drain:
   false}` before it beats or takes. A drain Mesh holds without the mark
   is not the node's to clear, and stays. If the resume fails, the mark
   stays and the next start asks again.
5. **SIGTERM is unchanged.** It is the fast stop, drain or no drain:
   every lease is handed back `node_restart` at once.
6. **The heartbeat is unchanged.** Mesh's heartbeat body is strict
   (unknown fields refused), so the node adds no `draining` field. Mesh
   learns of the drain from the drain call, and every heartbeat's answer
   then says `drain: true`. While draining, the node logs each beat with
   the leases it still runs.
7. **`mesh node status`.** It shows a v1 node from `node.status`: its
   pid, node id and version; whether it drains, for how long, and when
   its wait hands back; and each live lease with its job, kind and age.
   With no legacy node beside it, nothing else is printed.

Sessions that outlive a restart (`mesh node upgrade` for v1, the
legacy node's ADR-009 adoption) are not part of this amendment. Today a
lease's agent is a `podman run -i` child whose real stdin is a named
pipe and whose real stdout is a plain file, both under the lease's own
directory (`leases/<lease id>/`): the node writes to the pipe and
reads the agent's output by tailing the file from a saved offset,
rather than holding either end in an in-memory pipe of its own that a
restart would drop. Its credentials are in the node's memory: the DPoP
key, the session token, the interception CA, the canary values. Its
end state (the result held, the turn ledger, the access records) is
in memory too. Adopting it takes a detached agent, a sidecar that can
reload its credentials, and the lease's state on disk, so it is staged
separately. Until then, a deploy that should not restart work is a
drain.

## Amendment, 2026-11-03: `mesh node set` signals the running node

Changing a v1 node's capacity meant editing node.yaml and restarting
it — every lease in flight handed back `node_restart`, as the drain
amendment's SIGTERM path does. An operator who only wants to raise
`--cpus` on a busy node should not have to drain it first.

1. **`mesh node set --cpus N [--mem-gb N] [--slots N]`.** It rewrites
   node.yaml's own `v1.capacity` block — `cpus`, `mem_gb`, `slots` —
   with only the fields passed; one left out keeps node.yaml's own
   value for it (or none, if it never named one). This is never done
   through the legacy node's `internal/node.Config`: that type's
   `Save` marshals its own struct whole, which has no field for the
   top-level `v1:` key, and would silently drop it from node.yaml on
   write. `mesh node set` edits a generic YAML tree instead, touching
   only `v1.capacity.*` and leaving every other key — the legacy
   node's own included — exactly as it was.
2. **The signal.** With a v1 node running on this machine (its pid in
   `~/.mesh/node-v1/node.pid`, as the drain amendment left it), `mesh
   node set` sends it SIGHUP — the same signal the self-update's
   restart-and-adopt path reserved, free here because a v1 node has no
   self-update of its own yet. The node's capacity watcher rereads
   node.yaml at once and, when the total changed, calls the scheduler's
   `SetTotal`: `free` on the next take reflects it, no lease in flight
   is touched, and the process does not restart. With no v1 node
   running, `mesh node set` only rewrites node.yaml and says so; the
   change takes effect at the next start.
3. **Flags become defaults.** Once node.yaml names a capacity field —
   by `mesh node set`, or by hand — `mesh node run`'s matching flag
   (`--cpus`, `--mem-gb`, `--slots`) stops being read at startup for
   that field: node.yaml wins. A field node.yaml leaves out still takes
   the flag (or the flag's own default) as before. This is why `mesh
   node set` edits node.yaml rather than signalling a flag change
   directly: the running node and every later start agree on the same
   number without the operator having to edit a unit file too.
4. **Sandbox limits watch the same way.** `sandbox: {cpus, memory,
   pids}` in node.yaml (ADR-007) is reread on the same SIGHUP, live, at
   each pod's creation from then on; a pod already running keeps the
   limits it was given. `mesh node set` does not write this block today
   — only `v1.capacity` — but an operator hand-editing `sandbox:` and
   signalling the node (`kill -HUP`) sees it taken up the same way.
