---
summary: A Mesh v1 node (internal/nodev1) runs each lease as a job actor that owns everything the lease uses and leaves only through releasing; a scheduler alone owns the node's capacity as leases with a TTL and reports `free` on every take; a supervisor watches progress and reconciles the node every minute; a resource actor self-updates by draining, and the build cache is bounded live by entry, on by default. Each other piece lands behind a node flag, off by default.
status: proposed
---

# ADR-044: A v1 node runs each lease as a job actor

2026-10-30. Mesh v1 wave 3 (the actor node). Applies to `mesh node run`
(internal/nodev1) only; the legacy runtime (internal/node) is
unchanged. `mesh node run` carries no `--api` flag: it always runs
nodev1, and `mesh node drain`, `mesh node upgrade` and `mesh node status`
operate on the v1 node alone — a legacy node (`mesh node start`) left
running is told apart from none running at all by its pid, never
disambiguated by a flag (the drain and upgrade amendments below,
collapsed to the one v1 path).

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
5. **Resource actors** (internal/resources). Self-update drains the
   node, then hands back what still runs with cause `node_update`. The
   build cache is bounded by `WatchBuildCache`, evicting by entry beside
   live builds (amendment 2026-11-08); the exclusive cache trim it
   replaced is retired.
6. **No in-flight maps.** Once every piece is on, nothing in nodev1
   holds a lease's state outside its actor; a guard test holds it so.

## Rollout

Each piece is a PR behind a `mesh node run` flag, off by default:

| PR | Flag |
|---|---|
| 3.1 scheduler and the leases ledger | `--actors-scheduler` (with `--account-slots`) |
| 3.2 job actor, `actor.json`, panic recovery, `pod_killed` | `--actors` |
| 3.3 supervisor liveness and reconcile | `--supervisor=off\|warn\|cancel\|tidy` (live via node.yaml's `v1.supervisor`, amendment 2026-11-09) |
| 3.4 resource actors | `--self-update` (`--cache-trim` retired: the build-cache watcher runs by default, amendment 2026-11-08) |
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

1. **A pidfile of its own.** `mesh node run` writes its pid to
   `~/.mesh/node-v1/node.pid`. A legacy node and a v1 node run by one
   user keep apart: their state is in different directories. Beside the
   pid are:
   - `node.draining`, the draining marker;
   - `node.status`, the node's status (registration, drain, live
     leases). It is written at each heartbeat, as a lease starts or
     ends, and as the node drains.

   All three are removed as the process exits.
2. **SIGUSR1 drains.** `mesh node drain` signals the v1 node alone,
   never a legacy node.
   - With no v1 node running, `drainEither` looks for a legacy node
     (`mesh node start`, `~/.mesh/node.pid`) before giving up: finding
     one live, it refuses clearly, naming the legacy node's pid, rather
     than reporting "no Mesh v1 node is running" as if nothing ran on
     the machine at all — the legacy node keeps draining on its own
     signal (`kill -USR1 <pid>`) or by migrating it to `mesh node run`.
     With neither running, it is the plain "no Mesh v1 node is running"
     refusal.
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
   With no v1 node running and a legacy node running instead,
   `v1StatusLines` adds a line naming the legacy node's pid beside the
   "no Mesh v1 node running" line, rather than leaving the reader to
   mistake it for nothing running on the machine; with neither running,
   only that one line prints.

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

Unlike a drain, `mesh node upgrade` does not wait for a v1
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
   run`'s restart handler reads and removes
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

`mesh node upgrade` no longer swaps and restarts blind to
whether the hand-off above can actually pick a lease back up, or to
whether it did.

1. **A lease the hand-off cannot adopt refuses the upgrade.**
   `Status`/`LeaseStatus` (`internal/nodev1/drain.go`) now carries
   `Adoptable` per lease: false for one with no relay sidecar
   (`sidecarDir == ""`) or one whose tunnel intercepts --- the same two
   cases `Upgrade` (above) hands back `node_update` rather than
   writing a hand-off for. Before `(*nodeUpgrade).run`
   (`internal/commands/nodeupgrade.go`) swaps the binary, it reads the
   v1 node's status once (`V1Status`, nil when no v1 node is running —
   `mesh node upgrade` has no other node to read a status from) and,
   finding any lease not `Adoptable`, refuses: no binary swapped,
   no restart sent, the lease(s) named by id and job. With no v1 node
   running, there is nothing to refuse on this count; `v1Service.Restart`
   still swaps the binary first and then, finding a legacy node
   (`mesh node start`) running instead, reports it by pid rather than
   claiming the next `mesh node run` will pick up the new binary (the
   drain amendment's legacy-node detection, applied here too). `--force`
   swaps and restarts anyway, ending them, as `Upgrade` already would.
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
3. **`mesh node upgrade`'s refusal names the reason per lease**
   (`internal/commands/nodeupgrade.go`) when `AdoptableReason` is set,
   instead of the one fixed phrase every unadoptable lease used to get.
4. **On by default; `--cache-trim` retired.** `mesh node run` sets
   `BuildCacheConfigPath` to the node.yaml it already resolves for
   capacity (`node.ConfigPath()`) and `BuildCacheReload` to a channel
   the SIGHUP of `mesh node set` wakes beside the capacity and sandbox
   limits watchers, so every v1 node bounds its build cache with no flag.
   The exclusive trim — `Runtime.CacheTrim`, which held the build cache
   alone in the scheduler, and its `--cache-trim`, `--cache-dir` and
   `--cache-max-gb` flags — is gone from `internal/nodev1` and the
   command (`TestNodeRunWiresBuildCacheWatcherByDefault`).
5. **`Run` pins `BuildCache` to the cache the leases mount (2026-11-09).**
   `mesh node run` sets no `BuildCache`, and only `leaseCacheMounts`
   defaulted it — to `WorkRoot/build-cache`, locally — so the watcher
   ran with `BuildCache` "" and `evictBuildCache` returned before
   evicting anything: dl360's `~/.mesh/v1/build-cache` reached 456G
   against a default cap near 175G with no eviction ever logged.
   `diskGate` was live throughout (its floor read, free space read at
   `WorkRoot`, the same filesystem). `Run` now calls `pinBuildCache`
   before anything that may mount or watch the cache: `BuildCache` left
   "" becomes `buildCacheDir()` (`WorkRoot/build-cache`, the one path
   `leaseCacheMounts` mounts as `GOCACHE`) and is made. The watcher logs
   once at its first read (`build cache: watching <path> every <poll>:
   cap <n> GB, disk floor <n> GB`), and each eviction pass that removes
   entries logs the bytes before and after and the cap
   (`TestRunStartsTheBuildCacheWatcherOnTheMountedCache`).

## Amendment, 2026-11-08: `mesh node update` drains a v1 node beside the legacy one

`mesh node update` swaps the one executable both the legacy node and a
Mesh v1 node run (adr-002); it already drained a running legacy node
first (`drainIfRunning`, nodeupdate.go), but a v1 node running beside
it (`mesh node run --api v1`) was left untouched — still able to take a
new lease on a binary it was about to lose.

1. **`drainV1IfRunning`** (`internal/commands/nodev1state.go`) is
   `drainIfRunning`'s counterpart for the v1 node: with no pidfile under
   its run state (`node.ErrNoNode`) or a pid that is no longer alive,
   there is nothing to drain; otherwise it calls `drainV1Node` as `mesh
   node drain --api v1` does, with no wait.
2. **`nodeUpdate` gains `V1Run node.RunState` and `V1Drain func(node.RunState,
   io.Writer) (bool, error)`** (nodeupdate.go), defaulting to
   `v1RunState()` and `drainV1IfRunning` as `Run`/`Drain` already defaulted
   to the legacy node's own `node.DefaultRunState()`/`drainIfRunning`.
   `run()` drains the v1 node right after the legacy one, before the
   swap, and ORs its own "something was running" bool into the legacy
   drain's: either one being live is enough for the swap's closing lines
   to say the node is draining and its service will start the new binary,
   rather than "No node is running on this machine" — which was wrong
   whenever only the v1 node was up. Neither drain is conditioned on the
   other: both are unconditional, since the swap affects both regardless
   of which kind is running, and a `--api` flag would only ask the
   operator something the command can find out for itself.

## Amendment, 2026-11-09: tidy mode, and the supervisor's mode is a live node.yaml setting

`--supervisor` has carried a fourth mode, `tidy`, since 2026-11-03
(`internal/supervisor/supervisor.go`'s `ParseMode`, `Look` and
`Reconcile`), but it was never written up here, and every mode could
only be set at `mesh node run`'s start — changing it meant restarting
the node and re-adopting every lease.

1. **`tidy` reconciles like `cancel` but never escalates or stalls, like
   `warn`.** `Reconcile`'s `act := mode == Cancel || mode == Tidy`: a pod
   with no actor, a pod running only a sidecar, an orphaned job directory
   and a relay grant no actor holds are removed exactly as `cancel`
   removes them. `Look`'s escalate/stall ladder runs only `mode ==
   Cancel`; `tidy`, like `warn`, only logs what it would have done
   ("it would be stopped stalled") and leaves the lease running. Tidy is
   for a node whose disk and pods need sweeping without any risk to a
   session that is merely slow to report progress.
2. **`v1.supervisor` in node.yaml is read live, with no restart**
   (`internal/nodev1/supervisorconfig.go`, `supervise.go`'s
   `WatchSupervisor`). `ReadSupervisorConfig(path)` parses the same
   `off`/`warn`/`report`/`cancel`/`tidy` names `ParseMode` does; the key
   left out, or the file not existing yet, changes nothing. `Runtime.sup`
   keeps the one `*supervisor.Supervisor` `Run` built from
   `SupervisorMode` at start (set once, before `WatchSupervisor`'s
   goroutine starts, so it needs no lock there); the watcher calls its
   `SetMode` whenever a poll or a reload reads a mode that differs from
   the one last applied, and the very next `Look` or `Reconcile` runs on
   it. A supervisor that was off at start stays off: `Runtime.sup` is nil
   then, and the watcher does nothing — this turns a running supervisor's
   mode up or down, it does not switch one on.
3. **`mesh node run`'s `SupervisorConfigPath`/`SupervisorReload`** name
   the same node.yaml and the same reload channel the capacity, sandbox
   limits and build-cache watchers already share (`internal/commands/noderun.go`):
   editing node.yaml's `v1.supervisor` and sending the process SIGHUP —
   `mesh node set`'s signal, disambiguated from `mesh node upgrade
   --api v1`'s restart-and-adopt by the same `node.upgrade-ask` marker
   those watchers already rely on — changes the mode in force without
   ending or adopting any lease. The `--supervisor` flag's default
   (`off`) is unchanged; node.yaml's key only retunes a supervisor that
   flag already started.
4. **`TestTheSupervisorsModeIsReadLiveFromNodeYAML`**
   (`internal/nodev1/supervise_test.go`) runs it end to end on
   `actorRig`: starting in `warn` with an orphan pod and job directory
   standing and a live lease whose session stays quiet, it asserts
   nothing is removed and the lease is never escalated while still in
   `warn`; it then rewrites node.yaml's `v1.supervisor` to `tidy` and
   asserts the very next reconcile removes the orphan pod and directory
   while the live lease's own directory and pod are untouched, the
   `Runtime.Run` call never returns (no restart), and — even once the
   lease has gone quiet past twice `QuietAfter` — its session is still
   neither escalated nor handed back.

## Amendment, 2026-11-09: the legacy node is deleted; mesh-cli carries one node

Goal `the-v0-node-is-removed-from-mesh-cli` deleted the legacy runtime
this ADR's opening and Context once described as "unchanged beside" the
v1 node: `internal/node`'s `Loop`, `Runner`, `Evaluator`, `Planner`,
`Director` and `Rebaser` types, `internal/jobresult`, `internal/session`
and `internal/nodecaps` are gone, along with the `mesh node start` and
`mesh node sweep` commands. `mesh node run` (`internal/nodev1`, this
ADR) is now the only way this repo runs a machine as a Mesh node — not
one of two, the way the opening's "applies to `mesh node run` ... only;
the legacy runtime ... is unchanged" once distinguished it.

The opening, Context, and the drain/upgrade/status amendments below are
left exactly as written: a record of what wave 3 was built beside, not
of what mesh-cli runs today. Where they say a legacy node (`mesh node
start`) left running is told apart from none running at all by its pid
— never disambiguated by a flag, never acted on — that pid check is now
the only trace the deleted runtime leaves in this node's own code
(`internal/commands/nodev1state.go`'s `legacyNode`), kept solely so an
operator's stale `~/.mesh/node.pid` from before this goal reads as
"nothing running" rather than a second node. `internal/node` itself is
not fully deleted — the legacy gateway client `mesh mcp`, `mesh join`
and `mesh goal plan` still use, and a few of the old Loop's unreferenced
leftovers not yet swept up — but none of what remains there is a second
node; AGENTS.md has the detail.

## Amendment, 2026-11-15: a run_suite lane starts from the lease's warm cache

Goal `a-run-suite-lane-starts-from-the-leases-warm-cache`. A lease's
pod starts from a warm cache (`internal/nodev1/cachemount.go`, goal
`a-v1-lease-starts-from-a-warm-cache`): its tenant's GOMODCACHE, npm
cache and pnpm store are overlays whose read-only lower is the tenant's
shared cache and whose upper and work dirs are the lease's own, and the
node's one Go build cache is mounted read-write as GOCACHE. Those
mounts and their variables were set on the lease's pod alone
(`openLeasePod`). A lane opened beside it for run_suite (ADR-049) got
the lease's HOME and none of them: its `go test` read
`$HOME/go/pkg/mod` and `$HOME/.cache/go-build`, which the lease's pod
no longer fills, so every shard but the first started from an empty
module cache and an empty build cache.

1. **A lane mounts what its lease's pod mounts**
   (`Runtime.laneCacheMounts`): each tenant cache overlay at the same
   path over the same lower, the node's shared GOCACHE as it is, and
   the same `GOMODCACHE`, `GOCACHE`, `npm_config_cache` and
   `pnpm_config_store_dir`. They are read off the lease's session
   (`sandbox.Session.SharedMounts`, `ExtraEnv`), so a lease that runs
   without its tenant cache — the runtime refused the overlay
   (`cacheMountIsOff`, `runAgentRetryingCache`) — has lanes without it
   too, on the build cache alone.
2. **Each lane's overlay has an upper and work dir of its own**, under
   `<lease dir>/lanes/<n>/cache/<gomod|npm|pnpm>/`: an overlay's upper
   is never two pods' at once, so not the lease's and not another
   lane's. A lane therefore sees the tenant's shared cache, not what
   its lease's pod or another lane wrote since the lease began; a
   module the change itself adds is fetched once per lane, through the
   lease's relay. A lane's writes are not promoted to the tenant's
   cache (`PromoteAll` reads the lease's own upper): the lease's pod,
   which fetches the same modules, is what warms it.
3. **A lane's end removes its directory, and the lease's end removes
   every lane's with its own** (`removeLaneDirs`, from the lane's
   release and from the lease's pod's close, before the actor removes
   the job directory): through `sandbox.RemoveTree`, since an upper
   holds Go's module cache, read-only directories a plain `RemoveAll`
   leaves behind.

`sandbox.OpenBeside` itself still copies none of this: an overlay's
dirs are its caller's to make. `TestALaneMountsTheLeasesWarmCache`,
`TestALaneOfALeaseWithoutItsTenantCacheHasNone` and
`TestTheLeasesEndRemovesEveryLanesOverlayDirs` hold it.

## Amendment, 2026-11-16: a command step moves the lease's progress, and a trim leaves nothing free

Two leftovers of Mesh's queue goals (projectmesh-io/api #192).

**Step progress.** Mesh hands back a lease whose `progress_at` is its
stall limit old (`MESH_STALL_LIMIT`, 30 minutes; a heartbeat is not
progress). The node moved `progress_at` on a model session's stream
events alone (`Runtime.touch`), so a lease inside a 25-minute run_suite,
or a session waiting on one long node tool call, looked stalled while
it worked, and the limit could not be switched on.

1. **A command step moves `progress_at` too** (`Runtime.stepProgress`,
   `internal/nodev1/stepprogress.go`), at most once a minute per lease
   (`stepProgressEvery`), and the next heartbeat carries it. It moves:
   - while a run_suite unit writes to its stdout or stderr, a retry's
     output included, and on each test event
     (`validate.WithOnActivity`, `nodetools.RunSuite.OnActivity`;
     ADR-049's amendment of the same day);
   - while a lane opens or waits for its room (`validate.Busy`, every
     30 seconds until it has opened);
   - while any other node tool's call is in flight — push, apply_onto,
     sign_release and the rest — "still running <tool>" once a minute
     until the call answers, the session gives up on it or the lease
     ends (`Runtime.reportsWhileRunning`).
2. **A lease doing nothing moves nothing.** run_suite's call is left
   out of 1's last case on purpose: its call waits on a run that may be
   hung, and a command that is alive and silent is what a hung one
   looks like. A run that writes nothing shows Mesh no progress and the
   stall limit hands it back. The same holds for a suite that compiles
   for longer than the limit without a line of output: it states
   nothing the node can read, and should print as it goes.
3. **The supervisor reads the same clock.** `quiet_after` (decision 4)
   is measured from `progress_at`, so a session waiting on a suite that
   is writing is no longer escalated as quiet.

Not covered: a session's own long tool call (its Bash, not a node
tool), which the node sees only as silence between two stream events.

`TestARunSuiteWithOutputReportsProgressThroughout`,
`TestASilentRunSuiteReportsNoProgress`,
`TestARunSuiteThatGoesQuietStopsReporting` and
`TestANodeToolCallInFlightReportsProgressUntilItAnswers`
(internal/nodev1) and `TestRunTellsOfEachWriteAndOfNoneWhenSilent` and
`TestBusySaysSoUntilDone` (internal/validate) hold it.

**A trim never turns work away** (goal
`a-cache-trim-never-turns-work-away`, node half). What a v1 node runs
today is the build-cache watcher of the 2026-11-08 amendment:
`WatchBuildCache` evicts least-recently-used entries through
`resources.EvictLRU`, never takes the scheduler and so neither blocks
nor fails a running build. That half was already so. The exclusive trim
(`resources.CacheTrim`: wait up to 10 minutes for the cache alone, then
`go clean -cache`) is constructed nowhere in the node; only its type
and the scheduler's exclusive hold remain.

What was missing is the scheduler's half, for whatever holds the cache
exclusively: while an exclusive ask waited, shared asks were refused
but `Free` still answered the node's room, so Mesh offered leases the
node handed back `cannot_run` after `roomWait`.

4. **`Scheduler.Free` answers nothing free while a trim is pending or
   running** — CPUs, memory, slots and every account's sessions are 0
   while an exclusive ask waits or holds (`Scheduler.Trimming`) — and
   the take loop's `waitForSlot` waits it out, whatever the node's slot
   limit. The node asks Mesh for nothing during a trim, so nothing is
   handed back for one.

`TestFreeIsZeroWhileATrimIsPendingOrRunning` (internal/scheduler) and
`TestANodeTakesNothingWhileATrimIsPending` (internal/nodev1) hold it.

## Amendment, 2026-11-16: the reconcile spares new leases, an egress death is handed back pod_killed, and login keeps node.yaml

On dl360 (v1, `--actors --actors-scheduler --sandbox --supervisor
cancel`) three faults showed on 16 November.

**The reconcile took new leases' egress processes and pods for
leftovers.** The first reconcile after each start (06:51, 07:02) stopped
38 egress processes and removed pods of leases taken in the minute
before, and later in the day single leases were handed back seconds
after they began running. Two causes, both fixed:

1. **The live leases were read before the listings.** `Reconcile` read
   the fleet once, then listed pods (two podman calls, seconds on a
   loaded node), lease-egress processes, job directories and grants,
   and judged each listing against that early read. A lease the node
   took in between was judged unheld: its egress process was stopped
   (the lease was handed back `egress_died` seconds into running), and
   its git grant was closed under its checkout, so git answered
   `gitrelay: no such relay, or its token expired` and the lease went
   back `credential_timeout`. Each part now reads the live leases again
   after its own listing is in hand.
2. **A lease named its egress process only once its relay sidecar had
   started.** `setEgress` ran when `rs.open` returned. The process
   itself had started well before that, before the sidecar's start,
   install and readiness wait. `relaySidecar.onEgressStart` now names
   it the moment `cmd.Start` returns, to the actor (`NoteEgress`, so
   actor.json carries it from then) and to the supervisor
   (`running.egressStarting`, read by `fleet.Live`). It names it
   cleared again if the start then fails. `egressPID`, which an
   upgrade's adoptability and hand-off read, is still set where it was.
   An adopted lease counts its hand-off's egress process from the
   moment it is put among the leases.
3. **As a defence, a young lease is spared.** Nothing of a lease
   younger than `YoungAge` (`supervisor.DefaultYoungAge`, 5 minutes) is
   taken: not a pod whose lease's job directory or actor.json was
   written within it, and not an egress process that started within it
   (`EgressProcess.StartedAt`, read from `/proc/<pid>/stat`), and not a
   grant whose lease's job directory is that young. The
   reconcile line adds `spared N pod(s) and M egress process(es) of
   leases too young to judge` when it spares any.

**`egress_died` was never a cause Mesh took.** Mesh's hand-back enum
(projectmesh-io/api `service/meshgen/models_gen.go`, enforced in
`server/leases.go`) has no `egress_died`, so every such hand-back was
refused 422 and the lease was left to expire. This amends the
2026-11-08 amendment's point 1:

4. **An egress process dying on its own is handed back `pod_killed`.**
   Mesh documents that cause as the node's own fault, one the job may
   have caused (adr-001-amendment-infra-handbacks). It is counted, and
   it feeds the kind breaker. That fits a lease process killed under
   the lease. The reason names the egress process and its exit
   ("the lease's egress process died: it exited 1"). `CauseEgressDied`
   stays only so an older node's actor.json can be read back.
   `meshapi.MeshCause` sends it as `pod_killed`.
5. **The egress process exiting while its own lease is already ending
   is not a death.** `watchEgress` raises nothing once the lease's
   context is done, even when the exit and the cancel are seen at the
   same moment. The lease reports the cause that really ended it.
6. **Every cause the node sends is in Mesh's set.**
   `meshapi.HandBackCauses` and `meshapi.FailCauses` mirror Mesh's
   lists. `TestEveryCauseTheNodeSendsIsOneMeshTakes` reads every
   `meshapi.Cause*` that the node's source names, and checks each one
   against a copy of Mesh's enum. It also checks the session's
   `hand_back` causes.

**`mesh node login` rewrote node.yaml through the v0 `Config`.** That
struct knows nothing of `v1:`, `cache:` and the v1 node's other
sections, and it marshals `capacity` as a plain number. A login
therefore dropped `v1.capacity` and `cache` and wrote `capacity: 0`.

7. **Login edits only its two keys.** `node.SaveLatchkey` edits
   node.yaml as a YAML tree (`setTopLevelKeys`). It sets `latchkey` and
   `latchkey_client` where they stand, or appends them, and keeps every
   other key, its order and its comments. When nothing changed, it does
   not write. When it does write, it writes atomically through a temp
   file and a rename. `mesh node register` still saves through
   `Config` (v0 registration, unchanged here).

**A failed git mint was invisible.** A checkout whose git token was
not minted carried the mint's error after git's stderr. The reason is
clipped to 2000 bytes from its end, so the mint's error was cut off.

8. **The mint's error leads, and is logged on its own.** Every failed
   `MintSource` is logged in full, with the lease, repository, access
   and error. A checkout that failed for want of a token reads
   `checking out <repo>: its git token was not minted: <error>; git:
   <git's error>` (`mintFailedCheckout`).

`TestAMintFailedCheckoutKeepsTheMintErrorWhenClipped`,
`TestReconcileReadsTheLiveLeasesAfterListing`,
`TestReconcileSparesYoungLeases` (internal/supervisor),
`TestAReconcileWhileALeaseIsPreparedSparesItsEgressAndPod`,
`TestAReconcileMidPreparationSparesTheStartingEgressProcess` (linux),
`TestTheLeasesEgressProcessKilledEndsTheLeasePodKilled` (linux),
`TestAnEgressExitAfterItsLeaseEndedIsNotAnEgressDeath`,
`TestEveryCauseTheNodeSendsIsOneMeshTakes` (internal/nodev1) and
`TestSaveLatchkeyKeepsEverythingElse` (internal/node) hold it.

## Amendment, 2026-11-16: an adopted session reaches its model and node tools again, and its progress reaches Mesh

dl360 (v1.0.71-bootstrap.9) restarted its node at 23:48, 00:09 and
00:48 AEDT on 15–16 November. Each start logged "adopted N of N
hand-off(s) at start" (12, 21, then 25), and no adopted session made
progress again. node.log shows it for build lease `eb31087c` (job
`c57dc991`): its model calls ran every few seconds from 22:56 until
23:43:30, then stopped. After the 23:48:02 adoption, and after the
later two, not one model call was logged. The same held for all 12
leases adopted at 23:48: zero model calls after adoption, whatever
their kind. Mesh's stall reaper (`MESH_STALL_LIMIT`, 30 minutes) handed
them back, and those hand-backs opened the build breaker.

The relay's address was not the fault. The session reaches its relay
sidecar on the pod's own loopback, and the sidecar reaches the node on
the lease's socket (`<socket dir>/n`). Both outlive the node: the pod
and its sidecar keep running, and the adopting node listens on the same
path again. The fault was what the adopted socket served:

1. **No model forwarder and no node tools.** At a lease's start,
   `rs.open` serves the node's side of the socket with a `Proxy`: the
   lease's model account's forwarder at `/model/` and the kind's node
   tools at `/mcp/mesh-node`. That proxy lives in the node process.
   `adoptOne` built the node socket again with renewal, the Stop gate,
   the tunnel, Observe, Security and Canary, but no `Proxy`. So the
   sidecar forwarded every model call and every node tool call to a
   node that answered 404, an answer Claude Code does not retry. The
   session could go no further, and nothing told the adopted lease,
   which waited for a finish that could not come.
2. **Progress stopped at adoption.** A lease's `progress_at` moves on
   its session's stream events (`OnEvent`, read by the harness in the
   node that started the session). The adopting node has no harness,
   so `progress_at` stayed at the adoption time, and the reaper handed
   the lease back 30 minutes later however busy its session was.
3. **The hand-off named the wrong output file.** The hand-off noted
   the size of `agent-output.log`, a file nothing writes. The agent's
   real stdout is `agent.stdout` (agentio.go), so every hand-off's
   offset was 0 and adoption resumed nothing.

The product's MCP servers (served by the sidecar itself, on its
replaced credentials) and git (the checkout's origin rewritten onto
the sidecar's `/git/`) kept working through adoption. Only the routes
the sidecar forwards to the node itself broke.

So adoption now serves the node's side of the socket as a fresh start
serves it, as far as the hand-off allows:

4. **The model forwarder is rebuilt.** `adoptOne` reads the lease's
   model account again on the new session (`openAccount`). It serves
   the account's forwarder and the node tools on the adopted socket
   (`Runtime.nodeProxy`, shared with `rs.open` so the two cannot
   drift). The account's canary inspection and its vendor-refusal hook
   are wired as `work` wires them. An account that cannot be read hands
   the lease back `node_update`, like any other hand-off that cannot be
   adopted. The sidecar's AccessRecords are kept again (`Access`), so
   pushes made after a restart reach Mesh.
5. **The node tools that need nothing the old process held are served
   again.** These are `result`, `hand_back` and `progress`, through
   which a session ends its lease or reports how far it got, and
   `push`, which goes through the sidecar's control socket as every
   sidecar lease's push does (`adoptedNodeTools`). The rest are not
   served after a restart. `run_suite` and `stop_suite` need the lease's
   pod session, `apply_onto` the node's own git grant, `request_time`
   the lease's time budget, `escalate` the harness that read its
   mailbox, and `sign_release` the signer's session. A session calling
   one of those is told the node does not have it, and the node logs
   which tools it left out. Rebuilding run_suite for an adopted lease
   is a later item.
6. **The session's own output moves progress.** The hand-off notes how
   far into `agent.stdout` the node had read (`agentOutputPath`). The
   adopting node follows the file from there (`followAgentOutput`), and
   each line the agent writes moves `progress_at`, as each stream event
   did. Progress again stops only when the session stops writing.
7. **An adopted lease reports why it ended.** When the lease's context
   ends before a finish, adoption carries its cause into the outcome as
   `runActor` does. Mesh taking the lease back is reported as nothing,
   a cancel as `cancelled`, a second upgrade's hand-back as
   `node_update`, and a refused account as `account_refused`. Before,
   each of these fell through to a finish with no result (seen at 00:09
   as `422 evidence_required` on leases Mesh had already taken back).
   A session that hands its job back with `hand_back` is handed back
   with its own cause.
8. **The egress process is claimed before the socket serves.**
   `NoteEgress` and its hold now come before `node.Listen`, so no
   request rides an egress process that actor.json does not yet name.
   This was also why `TestNodeAdoptsHandoffAtStart` failed on main: its
   "with a live egress process" case read actor.json as soon as a
   CONNECT through the new socket had worked, which could be before
   `NoteEgress` ran.

Not covered: the adopting node does not see the session's end. A
session that exits without calling its finish or `hand_back` tool (a
validate session that cannot run run_suite after the restart, say)
leaves its lease until its output stops and the stall limit hands it
back, as before. Nor does the adopting node hold the lease's pod, so
the pod outlives the lease until the supervisor's reconcile removes it.

`TestASessionSurvivesANodeRestartE2E` (internal/nodev1) holds it, end
to end across real processes. A node process runs a lease whose relay
sidecar and stand-in agent run as processes of their own group, as the
pod's do. The agent keeps calling the model, the product's MCP server,
the node's `progress` tool and `git fetch` through the sidecar. The
node then gets SIGHUP and exits through `UpgradeSignal`, and a second
node process adopts the lease. The test asserts that every one of the
agent's calls after the adoption succeeds, that node B's heartbeats
carry a `progress_at` that moves past the adoption, and that the
lease's finish is reported once. On main the model and node-tool calls
fail 404 after adoption and `progress_at` stays put.
`TestNodeAdoptsHandoffAtStart` holds point 8.
`TestV1UpgradeAdoptsSidecarLeases` (internal/commands) now writes the
agent's output where the agent writes it.

## Amendment, 2026-11-16: the node reads its host's pressure, disk and temperature once a minute

The host gate read only load and free memory, and only when a take was
due. Nothing kept a picture of the host between takes.

1. **The reading is richer.** `MachineReading` (`hostgate.go`) now also
   carries `/proc/pressure/cpu` and `/proc/pressure/memory`'s `some`
   avg10 and avg60, the package temperature (the highest of every
   `x86_pkg_temp` or `TCPU` thermal zone, since a multi-socket host has
   one per package), and a `DiskReading` (free bytes, percent used) at
   the lease path (`WorkRoot`) and the cache path (`diskGatePath`).
   Each source has its own `Has` flag. A missing pressure file, thermal
   zone or unreadable disk path leaves its field zero with its flag
   false, never an error; only `/proc/loadavg` and `/proc/meminfo`'s
   `MemAvailable` must read.
2. **`HostProbe`** reads it (`ProcHostProbe{Root}`, `hostprobe_linux.go`;
   off linux it always refuses). `Root` lets a test feed a fixture
   `/proc` and `/sys`.
3. **`WatchHost`**, started by `Run` (with `ProcHostProbe{Root: "/"}`
   when `HostProbe` is nil), reads the probe at start and every
   `HostProbePoll` (a minute), caches the reading (`HostReading`) and
   keeps the last 60 (`HostHistory`, oldest first). It ticks whether or
   not a take happens. A failed read changes neither and is logged
   once, when reads start failing. The host and disk gates are
   unchanged and still read their own probes.

`TestMachineReadingCarriesPressureDiskAndTemp`,
`TestMachineReadingMissingSourcesAreZeroNotError` and
`TestRunReadsTheHostOnceAMinuteWithNoLeaseTaken` (internal/nodev1,
linux) hold it.

## Amendment, 2026-11-16: the heartbeat and node status carry the host reading, and the node backs off a hot package

`WatchHost`'s reading stayed on the node: Mesh never saw it, `mesh
node status` did not show it, and the host gate ignored the package
temperature it now read.

1. **The heartbeat carries it.** `HeartbeatRequest.Host`
   (`internal/meshapi`, `host`) is `WatchHost`'s latest cached reading:
   load, cores, free memory, CPU and memory pressure, package
   temperature, and lease and cache disk, each with its own `Has` flag.
   It is nil (omitted) before the first reading is cached, or with no
   `HostProbe`, never a zero reading. The heartbeat sends the cached
   reading and does not read the host itself.
2. **The host gate backs off a hot package.** `Host.PackageTempMaxC`
   (node.yaml's `host.package_temp_max_c`, carried by `nodeRunHost`
   as given, like `load_per_core_max`; absent or zero turns it off) holds back a new take while the gate's reading has a
   temperature over it: `host hot: package <t>°C over <max>°C`. A
   reading with no thermal zone never gates on it. The disk gate is
   unchanged. Both gates are checked independently before each take,
   each logs its own crossings once on entering and once on leaving,
   and neither touches a running lease.
3. **`mesh node status` shows it.** `Status.Host` and
   `Status.HostHistory` carry the cached reading and the last hour of
   readings. Once a reading exists, `DrainLines` adds `Host: …` (the
   current reading) and `peak last hour: …`. The peak line takes the
   worst value of each metric separately: the highest load, pressure
   and temperature, and the least free memory and disk. A source no
   reading carried is left out of the line rather than shown as zero.

`TestHeartbeatCarriesTheHostReading`, `TestHostAndDiskGatesTogether`
and `TestDrainLinesShowTheHostReadingAndItsPeak` (internal/nodev1)
hold it.

## Amendment, 2026-11-24: named reserves, a validation reserve with a person share, peaks by kind, and the reserve a waiter waits on

Goal `a-v1-node-reserves-room-for-validations-and-sizes-its-reserves-from-use`.
On 24 Nov dl360 logged 153 admission waits for validations: median 4
minutes, 90th percentile 22, longest 44. Builds and reviews filled the
admission budget, and hand pull request validations starved behind
automated item validations.

1. **The scheduler keeps named reserves.** The assistant's reserve
   (2026-11-24, #1953) is now one named reserve among others
   (`SetNamedReserve`; `SetReserve` is the assistant's). An ask that
   names its reserve (`Ask.Reserve`, or `Reserved` for the assistant's)
   draws what that reserve has free first, and the rest from the
   general room, which is the total less every reserve. No other ask is
   ever granted room in a reserve. The assistant's reserve is at most a
   quarter of each dimension, and all reserves together are at most
   three quarters, so the general room always keeps a quarter.
2. **The validation reserve.** This is node.yaml's
   `v1.validation_reserve` (`cpus`, `mem_gb`, `person_cpus`,
   `person_mem_gb`, `kinds`), or the `--validation-reserve-*` and
   `--validation-kinds` flags. The default kinds are `validate` and
   `evaluate`. A validation's lease hold and each of its lanes (admission
   and `holdLane`) draw it first. Each take names it to Mesh beside the
   assistant's, in the same `reserved`/`reserved_kinds`. A take carries
   one reserved room for all the kinds it names, so each reserve is
   named only while it has room for the largest lease of its kinds the
   node has held. The validation reserve is never named on a hot host's
   take, or when no job slot is free. With no room set, the node
   admits as before.
3. **The person share.** This is room inside the validation reserve
   that only a person's pull request validation draws. The node applies
   runsheet's `validate_for=<repo>:person` rule itself: the brief's
   subject is a `pull_request`, and the repository is
   `code.repo.full_name`. A person's all-or-nothing lane take waits
   ahead of every automated one. Among people's validations, a
   repository holding none of the share goes ahead of one that holds
   some, so each repository gets a lane before any repository gets a
   second.
4. **Peaks by kind.** Every 15 seconds the node runs `podman ps` to get
   each container's name and pid, then reads that pid's cgroup:
   `cpu.stat` `usage_usec`, `memory.current` and `memory.peak`. Each
   lease keeps in its record the most cores its containers used
   together over one reading's window, and the larger of their summed
   `memory.current` and any one container's `memory.peak`. At the
   lease's end these fold into its kind's median, 95th percentile and
   highest. The kind also records how many of its leases ran at once.
   All of it appears in `mesh node status` (`usage`), and node.log gets
   a line per lease. A node without the sandbox reads no peaks and
   still counts its leases.
5. **The holding report names the reserve.** `mesh node status`
   (`waiting`, `reserves`) lists every ask waiting for room, with the
   room it waits on, what it asks, since when and why. It also lists
   each reserve's room and what it has free. The admission and lease
   wait lines in node.log name the reserve too.

`TestAValidationReserveIsNeverTakenByBuilds`,
`TestAHandPRValidationIsNotStarved`, `TestLeasePeaksAreRecordedByKind`,
`TestHoldingNamesTheReserveALeaseWaitsFor`,
`TestNoValidationReserveAdmitsAsBefore` (internal/nodev1), and
`TestPersonValidationsTakeTurnsByRepo` and `TestReservesAreBounded`
(internal/scheduler) cover it.

## Amendment, 2026-11-25: a waiting gate validation goes ahead of new builds

Goal `a-waiting-gate-validation-goes-ahead-of-new-builds-on-the-node`.

1. **Ask.Build and Ask.Gate.** A Build ask is an ordinary build; a Gate
   ask is a merge-gate validation. While any Gate ask waits in Take for
   room, a new Build ask is refused (`behind gate validation`) rather
   than claiming the general room piecemeal as it frees: it is admitted
   only once the general room has, all at once, enough free for every
   waiting Gate ask's need, so a stream of builds taking room a little
   at a time never leaves a gate validation waiting forever for the
   room it needs in one piece. The waiting Gate ask's own Take then
   proceeds ahead of any further build (`buildgate.go`,
   `buildBehindGateLocked`).
2. **Scheduler.BuildAging.** A Build ask (by its Owner, across tries)
   held behind a waiting Gate ask for longer than `BuildAging` (0 never
   ages one out) is let through anyway, Logf hearing it, so a slow or
   oversized gate validation cannot stall every build indefinitely.
3. **Scope.** This lands the scheduler primitive alone
   (`internal/scheduler`); no job kind in `internal/nodev1` sets
   `Ask.Build` or `Ask.Gate` yet, so it is additive and inert in the
   running node until a later item wires a build lease's and a
   merge-gate validation lease's own ask to it, alongside the existing
   `validationReserved`/`reserveAsk` kind dispatch.

`TestANewBuildIsRefusedBehindAWaitingGateValidation` and
`TestABuildPastItsAgingIsAdmittedAheadOfAWaitingGateValidation`
(internal/scheduler) cover it.
