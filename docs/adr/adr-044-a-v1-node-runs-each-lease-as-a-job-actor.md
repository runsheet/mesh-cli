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

## Amendment, 2026-10-30: the self-update confirms or rolls back

The legacy node's rollback (internal/node selfupdate.go, mesh-specs ADR
002) is ported to the v1 node, behind the same `--self-update`:

1. **Recorded before the swap.** The release updater records the update
   (`from`, `to`) in the v1 node's own state file,
   `<work-root>/update.json` — never the legacy node's
   `~/.mesh/update.json`, which a legacy node of the same user may be
   confirming at the same time — before it swaps the binary, the old one
   kept as `<exe>.prev`. A release that fails to fetch, verify or swap is
   recorded with why.
2. **Every start counts.** A start of the new binary counts itself
   against the pending update and logs it first: `confirming update vX →
   vY: start 1 of 3, rolls back on the third`.
3. **The first heartbeat confirms.** It is sent at once on an
   unconfirmed start; Mesh answering it confirms the update. A start
   that cannot within the confirm window (`--confirm-within`, 5 minutes)
   ends with an UnconfirmedError naming the last refusal, and the
   service manager's restart is the next start.
4. **The third unconfirmed start rolls back.** It puts `<exe>.prev` back
   and exits 0 before it registers, for the service manager to start the
   previous binary. A version rolled back is never due again; a newer
   release is.
