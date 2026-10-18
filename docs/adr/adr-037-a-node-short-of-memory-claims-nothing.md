---
title: "ADR-037: A node short of memory claims nothing"
summary: node.yaml's host.min_free_memory (a size such as 32g, or a share such as 15% of MemTotal) is a memory floor; while it is set, every tick before any claim the node reads MemAvailable and, at or under the floor, claims nothing of any kind — as a node low on disk does (ADR-035) — logging `memory_low: …; claiming nothing` and `memory ok: …; claiming again` once per crossing; the ranged scalers take the same floor for their growth cap; left out, nothing changes from ADR-004's 2 GiB growth cap.
status: accepted
---

# ADR-037: A node short of memory claims nothing

2026-10-18. Sits beside [ADR-035](adr-035-a-node-low-on-disk-claims-nothing.md)
(a node low on disk claims nothing) and amends the free-memory cap of
[ADR-004](adr-004-capacity-scales-with-headroom.md) and
[ADR-005](adr-005-capacity-across-accounts.md), which
[ADR-023](adr-023-the-node-backs-off-a-hot-host.md) kept on growth.

## Context

ADR-004 caps a ranged node's growth at the sessions it runs when
`MemAvailable` is 2 GiB or less (`MinFreeMemory`), a constant sized for
the mini, where CI shares the machine. It never stops a claim below the
capacity in force, and a fixed `capacity:` is not judged at all.

dl360 runs two nodes (`dl360-anythink`, capacity 12, and
`dl360-grapevine`, capacity 8) on 251 GiB, each session in a pod capped
by `sandbox.memory`. On 2026-10-18 the anythink node logged 208
validation steps killed with exit 137 in one day — unit, integration and
scan steps hitting the pod's 16g cap, some 75 seconds in. The pods were
raised to 32g that evening; at twenty sessions that is up to 640 GiB of
caps on 251 GiB, and a 2 GiB floor only notices once the host is all
but gone. Each kill was reported as the item's failure, re-planned and
claimed again on the same machine.

## Decision

- **A floor the node sets.** node.yaml `host: {min_free_memory: 32g}`
  (`HostConfig.MinFreeMemory`): a size — `32g`, `32G`, `32gb`, `512m`,
  `1024k`, or a bare byte count — or a share of the machine's memory,
  `15%`, of `/proc/meminfo`'s `MemTotal` (now read beside `MemAvailable`,
  `MachineReading.MemTotal`). `HostConfig.MemoryFloor(total)` is it in
  bytes. A zero size, a share at 0 or at 100 and more, or anything
  unreadable is refused at load naming `host.min_free_memory`. A share on
  a machine whose total was not read is `MinFreeMemory`. Reloaded every
  tick with the rest of `host:`, a change logged `host: min_free_memory
  (none) → 32g`.
- **Read every tick, before any claim, when set** (`Loop.memFloor`,
  `internal/node/memfloor.go`, after `diskFloor`): the loop's `Machine`
  probe, `MemAvailable` against the floor (`ShortOfMemoryAt`). At or
  under it the node claims nothing of any kind: `Tick` treats it as it
  treats a low disk — no evaluation, rebase, plan, research, merge batch
  or main validation, no build. Main validations and merge batches are
  the memory-hungriest claims; a floor that held only builds would miss
  them.
- **Running sessions are never touched**, as on a low disk.
- **Said once per crossing**: `memory_low: 20.0GiB free <= floor
  32.0GiB; claiming nothing` and `memory ok: 100.0GiB free > floor
  32.0GiB; claiming again`. A failed read keeps the last judgement and is
  said once per distinct error. While low, `Loop.MemoryFindings()` is a
  critical `memory_low` Finding, as `DiskFindings` is for the disk.
- **The scalers take the same floor.** `Scaler` and `AccountsScaler`
  carry the loop's `host:` (`Host`, set from `HostThresholds` before each
  `Target`), so their `low memory` growth cap is judged by it too.
- **Left out, nothing changes.** With no `min_free_memory` the floor
  does not run: the scalers' cap stays at `MinFreeMemory`, a fixed
  capacity is not judged by memory, and a node that crossed and then had
  the key removed says `memory ok: host.min_free_memory is not set;
  claiming again`. ADR-004's tests stand as they were.

## Consequences

- A host short of memory costs no attempts: items stay ready or
  requested on the platform for another node, or for this one once
  sessions finish.
- The floor is per node, not per host: two nodes on one machine read the
  same `MemAvailable` and both stop, which is what a shared machine
  wants.
- The floor has no hysteresis: a host hovering at it may cross each
  tick. The ranged scalers' five-minute step smooths their side; the
  claim floor relies on sessions taking minutes to change memory use.
  If it flaps in practice, a quiet band as ADR-031 gives the host
  back-off is the next step.
- A sensible floor is at least one pod's `sandbox.memory`, so a claim
  never starts a session the host cannot hold. dl360 is to set `32g`.

## Tests

`internal/node/memfloor_test.go`: sizes and shares parse and bad values
are refused naming the key; node.yaml carries the key; `ProcMachine`
reads `MemTotal`; the scaler's growth cap takes the configured floor;
three ticks under a set floor over a build, an evaluation, a plan, a
rebase and a direction claim nothing and log one `memory_low`, then
claim once memory is freed and log one `memory ok`; a merge batch is not
claimed under the floor; with the key left out a node with 1 GiB free
claims as before and says nothing of memory.
