---
summary: A node backs off a hot host — every tick, while the 1-minute load per core is past node.yaml's host.load_per_core_max (1.5) or the package temperature past host.package_temp_max_c (90°C), the capacity in force drops one slot to max(min, 1) and nothing new is claimed; quiet, it climbs one slot per five quiet minutes back to the plan figure (the scaler's, or a fixed N), never above; the reason is `host hot: load 55/18 cores` or `host hot: 101°C` and the scaling report carries the host's load, cores and temperature. Replaces ADR-004's and ADR-005's machine-load growth cap.
status: accepted
---

# ADR-023: The node backs off a hot host

2026-10-03, goal `the-node-backs-off-a-hot-host` (item 3). Amends
[ADR-004](adr-004-capacity-scales-with-headroom.md) and
[ADR-005](adr-005-capacity-across-accounts.md).

## Context

On 2026-10-03 at 08:35Z the mini was at load 27 / 55 / 71 on 18 cores,
its package at 101°C and throttling: four GitHub Actions runners, two
nodes (capacity 8 and 4) and a QEMU image build share it, none knowing
of the others. Capacities were cut by hand and nothing raised them
again once the box was quiet.

The scalers had one view of the machine: no growth past the running
count while the load was at or above one per core (`machine load`).
That never shrinks a node, does nothing for a fixed `capacity: N`, runs
once a minute at most, and knows nothing of temperature.

## Decision

- **Thresholds.** node.yaml `host: {load_per_core_max: 1.5,
  package_temp_max_c: 90}`, defaults filled, reloaded every tick. The
  host is read every tick (`MachineProbe`: `/proc/loadavg`, the CPU
  count, the `x86_pkg_temp` or `TCPU` thermal zone); it is **hot** when
  the 1-minute load per core or the package temperature is past its
  threshold (`MachineReading.Hot`), load named first.
- **A layer over the plan figure** (`internal/node/hostbackoff.go`).
  The plan figure is what the capacity was before: the scaler's or the
  accounts scaler's stepped capacity for a range, `N` for a fixed node.
  The back-off holds `backedOff` slots of it back; the capacity in
  force is the plan less those, never below the floor — `max(min, 1)`
  for a range, 1 for a fixed node — and a plan already under the floor
  as it is.
- **Down one slot per tick while hot**, every tick (`Loop.hostBackoff`,
  after `scale` and before any claim), not once a minute: a hot host
  needs relief now. While hot **no new item is claimed**; running
  sessions are never stopped.
- **Up one slot per five quiet minutes** (`HostRecoverEvery`): the
  quiet spell starts at the first quiet tick and starts again after
  each slot given back; a hot tick ends it. Never above the plan figure.
- **Said once per step**: `capacity 4 → 3: host hot: load 55/18 cores`,
  `capacity 3 → 4: host quiet 5m`.
- **Reported.** `Scaling` carries `Host {load, cores, tempC, hasTemp,
  hot}` as last read. While held back or hot the reason is the host's
  (`host hot: load 55/18 cores`, `host hot: 101°C`); level again it is
  the plan's. A fixed node reports scaling (range 1–N) while held back
  or hot, and `fixed` once level and quiet.
- **Replaces the machine-load growth cap.** `ReasonMachineLoad` is
  removed from `Scaler.Target` and `AccountsScaler.Target`; the
  free-memory cap (`MinFreeMemory`) stays on growth with its own reason,
  `low memory`.
- No probe (`HostMachine` off Linux) or a failed read changes nothing.

## Consequences

- A node sharing a box with CI gives slots back within minutes of the
  box getting hot, and takes them back on its own once it cools — the
  hand cut of 2026-10-03 is no longer needed.
- A fixed node backs off too; `capacity: N` is its ceiling, not a
  promise.
- The pace's hard stops and the plan's windows stay above the layer:
  the back-off only ever lowers what they allow.
- The heartbeat's `scaling` and `mesh node status` show the host
  figures once the gateway's schema takes them — later items of the
  goal; until then the reason alone rides the existing `scaling.reason`.
