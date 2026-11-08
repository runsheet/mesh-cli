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

A deploy that should not restart work is still a drain; `mesh node
upgrade` and `mesh node set` are the amendments below.

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
   node set` sends it SIGHUP — the same signal `mesh node upgrade`
   sends for its restart-and-adopt (the amendment below).
   `node.upgrade-ask`, written only by that command right before it
   signals, is what tells the two apart; `mesh node set` writes
   nothing there, so the running node's restart handler finds no
   marker and takes the signal as a reload. It rereads node.yaml at
   once and, when the total changed, calls the scheduler's
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
   signalling the node (`kill -HUP`) sees it taken up the same way,
   provided no `node.upgrade-ask` is sitting there from an upgrade
   whose handler never ran.

## Amendment, 2026-11-04: `mesh node upgrade` for v1 (sessions survive a restart)

Unlike a drain, `mesh node upgrade --api v1` does not wait for a v1
node's leases to end: it leaves every one running, under the agent
that is already running it, and restarts the node process at once so
the binary already swapped onto disk (adr-009's restart, which carries
the legacy node the same way) picks them back up.

1. **The signal is SIGHUP, marked.** `v1Service.Restart`
   (`internal/commands/nodeupgrade.go`) reads the v1 node's pidfile the
   same way `drainV1Node` does, writes `node.upgrade-ask` (empty,
   `0o600`, beside the pidfile) and only then sends SIGHUP — the same
   signal `mesh node set` sends for its live reload (the amendment
   above), so SIGHUP alone no longer says which is meant. `mesh node
   run --api v1`'s restart handler reads and removes
   `node.upgrade-ask` first (`takeV1UpgradeAsk`, the same
   read-once-and-clear shape as the drain wait's `node.drain-wait`):
   found, it calls `rt.UpgradeSignal(...)`; not found, it is `mesh node
   set`'s reload and the handler pushes to the capacity and
   sandbox-limits reload channels instead. `v1Started` and `v1Stopped`
   remove the marker the same way they remove `node.drain-wait` and
   `node.status`, so one left over from a run that never reached its
   handler is never read by the next.
2. **The hand-off, and a bounded wait for hand-backs.**
   `Runtime.UpgradeSignal` (`internal/nodev1/upgrade_signal.go`) calls
   `Upgrade(why)`. `Upgrade` (`internal/nodev1/handoff.go`) locks the
   lease table and, for each live lease, writes `handoff.json` under the
   lease's own directory atomically: its brief, its sessionproxy socket
   directory, its push heads, its sidecar directory and its turn
   ledger. On success nothing about the lease is touched --- its pod,
   sidecar and agent keep running exactly as they were, and nothing is
   yet reported to Mesh. A lease that intercepts, or whose hand-off
   cannot be written, is handed back `node_update` instead (the drain
   wait's cause), same as before --- and `Upgrade` returns the ids it
   handed back this way. `UpgradeSignal` then waits, bounded, for those
   hand-backs (and only those; a plain hand-off needs no such wait) to
   be reported to Mesh before exiting the process (`UpgradeExit`,
   `os.Exit`, swapped in tests): `awaitReported` polls the lease table
   until every handed-back id is gone from it --- `runActor` deletes a
   lease's id only after its actor has reported its end --- or until
   `handBackWait` (twice `reportTimeout`) elapses, whichever comes
   first. An `os.Exit` cut off mid-report would leave its lease's
   `actor.json` `Cancelling` and unreported on disk forever, since a
   hand-off is never adopted that way; the wait is what keeps that from
   happening. Neither the wait nor the exit cancels the root context
   every lease's own descends from, which would end every lease the
   upgrade means to leave running.
3. **Start-up adoption picks it back up.** The process that starts next
   (`adoptHandoffs`, `internal/nodev1/adopt.go`) reads every hand-off
   left under the leases directory, each on its own goroutine, and
   replumbs the node's own side of it from nothing: a fresh session (a
   new DPoP key, never the hand-off's old one, so a process that no
   longer exists is never again mistaken for this one), the sidecar
   pushed that key as the lease's credential from now on, its room in
   the scheduler taken again, and its agent's output tailed from the
   hand-off's saved offset. The lease's egress tunnel, sessionproxy,
   token renewal and Stop gate are not reattached -- rebuilding those
   means rebuilding the pod session itself, which adoption does not do
   -- so adoption only goes far enough to observe the finish, carry
   security reports and scan for the lease's injected values. A
   hand-off that cannot be adopted this way is handed back `node_update`
   the same way, its job directory removed; `adoptHandoffs` reserves
   each hand-off's lease id synchronously, before any adoption's
   network-bound work starts, so the start-up sweep that removes
   leftover job directories never races one still being adopted.
4. **The lease finishes on its own, under either process.** Once the
   agent answers, the lease that adopted it reports and releases exactly
   as `runActor`'s own tail does, so Mesh sees one finish (or hand-back)
   for the job, never one from each process.

## Amendment, 2026-11-06: the upgrade refuses an unadoptable lease, and reports what the new process adopted

`mesh node upgrade --api v1` no longer swaps and restarts blind to
whether the hand-off above can actually pick a lease back up, or to
whether it did.

1. **A lease the hand-off cannot adopt refuses the upgrade.**
   `Status`/`LeaseStatus` (`internal/nodev1/drain.go`) now carries
   `Adoptable` per lease: false for one with no relay sidecar
   (`sidecarDir == ""`) or one whose tunnel intercepts --- the same two
   cases `Upgrade` (above) hands back `node_update` rather than
   writing a hand-off for. Before `(*nodeUpgrade).run`
   (`internal/commands/nodeupgrade.go`) swaps the binary, it reads the
   v1 node's status once (`V1Status`, nil for a legacy-node upgrade)
   and, finding any lease not `Adoptable`, refuses: no binary swapped,
   no restart sent, the lease(s) named by id and job. `--force` swaps
   and restarts anyway, ending them, as `Upgrade` already would.
2. **The swap reports what the restart actually adopted.** When every
   lease was adoptable (or `--force` overrode a refusal) and at least
   one lease was running, `run` polls `V1Status` after
   `Service.Restart` returns until it sees a status from a different
   pid than the one running before the swap, then prints how many of
   the leases left running the new process's own status says it
   adopted. Fewer adopted than were left running is reported as a
   failure (a non-zero exit), not a quiet success; a new status never
   appearing within the poll's timeout is the same. This is
   `mesh node upgrade`'s own account of step 3's adoption, not a
   change to adoption itself.

## Amendment, 2026-11-08: the lease's egress process has its own cause and its own actor.json field

A lease's egress process (ADR-051 decision 1, mesh-specs) runs in its
own process group, outside the node's and outside the pod's: nothing
but its own exit, or a signal sent from outside the node, ends it.
Decision 2's closed set of hand-back causes had no way to say "the
egress process died" apart from `pod_killed` (an agent exit) or
`stalled` (the supervisor's own escalation) — neither of which this
is.

1. **`egress_died`** (`meshapi.CauseEgressDied`) is added to Decision
   2's set of causes: the lease's egress process ending on its own
   while the lease is still live, never caused by the actor stopping
   it as part of releasing. Like `pod_killed`, it is the node's own
   condition, not the work's — Mesh takes it as a hand-back, not a red
   verdict. It is distinct from `pod_killed` (the agent's own container
   exiting) and from `stalled` (the supervisor escalating on silence):
   the relay sidecar's `watchEgress` raises it the instant the egress process's wait
   returns, whether or not the agent or supervisor ever notice, and it
   ends the lease immediately rather than waiting for `quiet_after` or
   an agent exit to be observed.
2. **`actor.json` names the lease's egress process.** `Record.EgressPID`
   and `Record.EgressControl` (json `egressPid`, `egressControl`) are set
   by `Actor.NoteEgress` before the actor leaves `preparing`, once the
   lease has an egress process to name — the same point `Holds` and
   `Pods` are already kept up to date for the leftovers path. A node
   that stops without releasing the lease leaves it on disk; the next
   start's `releaseLeftovers` (Decision 2, "a node that stops without
   releasing leaves `actor.json` behind") reads `Record.EgressPID` beside
   `Record.Pods` and stops the named process (`stopLeftoverEgress`: its
   own process group, SIGTERM then, after a bounded wait, SIGKILL)
   after removing the lease's pods and before removing its job
   directory — so an egress process a dead node left running never
   outlives the next node's clean-up, whether or not that lease's own
   `watchEgress` is still there to catch it.

## Amendment, 2026-11-08: the build cache is bounded and read live, and a near-full disk gates new leases

The Go build cache the node's leases share (`Runtime.BuildCache`) had
no live cap of its own, and nothing in `internal/nodev1` read how
full the disk under it was before taking another lease.

1. **`cache: {max_gb, min_free_gb}`** is a new, top-level node.yaml
   section of v1's own — the legacy node's config names no top-level
   `cache:` key, so this clashes with nothing — read directly in
   `internal/nodev1/cachewatch.go` (`ReadBuildCacheConfig`,
   `nodeYAMLCacheFields`) the way `capacitywatch.go` reads `v1.capacity`,
   since `internal/nodev1` may not import `internal/node`
   (`guard_test.go`). `max_gb` bounds the node's build cache; a
   node.yaml that leaves it out defaults it to `DefaultCacheFreeShare`
   (60%) of the free space `Disk` reads at the cache's own path.
   `min_free_gb` is the new disk gate's floor (below); left out, it
   defaults to `DefaultMinFreeGB` (50).
2. **`WatchBuildCache`** (`Runtime.WatchBuildCache`, started from `Run`
   when `BuildCacheConfigPath` is set) rereads the `cache` section every
   `BuildCachePoll` and at once on `BuildCacheReload`, keeps it live as
   `r.buildCacheConfig`, and evicts `BuildCache`'s least-recently-used
   entries toward `max_gb` through `resources.EvictLRU` (the entry-by-entry
   evictor the goal before this one added) whenever it reads over the
   cap. This runs by entry, never taking `r.Scheduler`, so it evicts
   alongside a lease still building against the same cache — unlike the
   wholesale `go clean -cache` the legacy node's scheduled `CacheTrim`
   still does.
3. **`diskGate`/`noteDiskGate`** (`internal/nodev1/diskgate.go`) is a new
   gate of the same shape as `hostGate`/`noteHostGate`, checked once per
   pass of `Run`'s take loop right after `hostGate` and before
   `r.API.Take`: "" while free space at the build cache's own path (or
   `WorkRoot`, with no `BuildCache` set), read through the new `Disk`
   field (`DiskProbe`, `FSDiskProbe` on linux via `statfs`), stays at or
   above `r.buildCacheConfig`'s live `min_free_gb`; a reason naming the
   path, the free space and the floor otherwise. A lease already running
   is never touched by it — only a new one waits. `noteDiskGate` logs a
   crossing alone, as `noteHostGate` does, and the gate's current reason
   now also rides `Status.DiskGate` and a `DrainLines` line ("Disk:
   ...; taking no new lease."), the first gate of this shape surfaced
   through `mesh node status` rather than kept purely internal. A nil
   `Disk` (the watcher off) or a floor read as 0 never gates.

## Amendment, 2026-11-08: an intercepting lease with a live egress process is adoptable

The 2026-11-06 amendment's `Adoptable` treated every intercepting lease
alike: its CA trusted nowhere but this process's memory, so no later
process could ever pick it back up. That was only true of a lease whose
relay ran in process. ADR-051 decision 1's egress process holds the CA
and the policy itself, outside this process, reachable over its own
control socket -- so an intercepting lease whose egress still runs as
that separate process when a later process starts is, in principle,
exactly as adoptable as any other sidecar-mode lease; only one with no
egress process of its own (in-process relay) or whose egress process has
since died is not.

1. **`adoptability` (`internal/nodev1/drain.go`) replaces the flat
   `sidecarDir != "" && !intercepts` test.** A lease with no relay
   sidecar is still never adoptable. A non-intercepting sidecar-mode
   lease still is. An intercepting lease is adoptable when its
   `egressPID` names a process that is still running (`egressAlive`,
   `internal/nodev1/leaseegress.go`: `kill(-pid, 0)` against its own
   process group, the same signal `stopLeftoverEgress` sends it) --
   never when `egressPID` is 0 (an in-process relay, never started one)
   nor when the named process has died. `Status` carries the result as
   before, now beside a new `AdoptableReason`: empty unless an
   intercepting lease's own egress process is the reason it is not
   adoptable, in which case it names that process.
2. **`Upgrade` (`internal/nodev1/handoff.go`) hands off, rather than
   back, an intercepting lease `adoptability` finds adoptable.** Every
   other lease is handed back `node_update` exactly as before --
   nothing about step 3's adoption (picking the egress process itself
   back up, over its control socket) is built here; this only stops
   ending, needlessly, a lease that an adopting process could in
   principle resume.
3. **`mesh node upgrade --api v1`'s refusal names the reason per lease**
   (`internal/commands/nodeupgrade.go`) when `AdoptableReason` is set,
   instead of the one fixed phrase every unadoptable lease used to get.
