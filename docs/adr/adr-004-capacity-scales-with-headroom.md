---
summary: node.yaml capacity takes a plain number (fixed) or {min, max}; a ranged node steps one slot at a time, at most every 5 minutes, toward what the plan's five-hour (target 0.90) and seven-day (target 0.95) windows can afford at its own measured burn per slot, capped by the machine's load and free memory, and reports it as the heartbeat's optional `scaling`.
status: accepted
---

# ADR-004: Capacity scales with the headroom of the plan's windows

## Context

A node's capacity has been one number in `~/.mesh/node.yaml`, re-read
every tick. A fixed number is wrong both ways: set high, the Max plan's
windows run out (on 2026-09-23/24, 51 of 69 item failures were its
limits); set low, a week with room to spare goes unused. Goal
`node-reads-its-limits` (trd §1 Node `limits`, §3 Pacing by the plan's
limits) gives the node the plan's windows from every session's
`rate_limit_event` — `five_hour` and `seven_day`, each a `utilization`
(0–1) and a `resetsAt` — and the hard stops on them. Goal
`node-scales-with-headroom` (Chris, 2026-09-25) asks the node to grow
when those windows have room and shrink when they do not. The mesh-specs
repo cannot be reached from this goal's items, so the decision is
recorded here.

## Decision

### Capacity is a number or a range

`capacity` in node.yaml takes either shape:

```yaml
capacity: 3              # fixed, as today: min = max = 3
capacity: {min: 1, max: 4}
```

A plain number behaves exactly as before and saves back as the same
plain number; `mesh node register --capacity N` writes one. A range must
have `min ≥ 1` and `max ≥ min`, or node.yaml does not load. Only a range
scales; a fixed node never runs the scaler (`internal/node/config.go`,
`Capacity`).

### The rule

Every minute a ranged node computes a target (`internal/node/headroom.go`,
`Scaler`):

1. **Burn per slot**, per window: the rise in that window's utilization
   over the last 30 minutes (`BurnWindow`) divided by the slot-minutes
   run in that time. Before there is data — fewer than two readings, no
   slot-minutes run, or the window reset in between — it is the default,
   `DefaultBurnFiveHour` (a slot spends the five-hour window in five
   hours of running, 1/300 per slot-minute) and `DefaultBurnSevenDay` (a
   slot spends the week in forty, 1/2400). It is never below the floor,
   `FloorBurnFiveHour` and `FloorBurnSevenDay`, a hundredth of the
   defaults, so a quiet half hour does not read as endless slots.
2. **Affordable rate**, per window: (target − utilization) / minutes to
   its `resetsAt`. The targets are **0.95 for `seven_day`**
   (`SevenDayTarget`: the week is spread, not spent on day one) and
   **0.90 for `five_hour`** (`FiveHourTarget`). A window at or over its
   target affords nothing; a window with no reading, or past its reset,
   sets no bound.
3. **Slots**, per window: affordable rate / burn per slot, rounded down.
4. **Target capacity**: the smaller of the two windows' slots, clamped
   to `[min, max]`.
5. **The machine**: CI runs on the same mini, so when the 1-minute load
   average is at or above the CPU count, or `MemAvailable` is at or
   below 2 GB (`MinFreeMemory`), the target is capped at the sessions
   already running — no growth (`internal/node/machine.go`, read from
   `/proc/loadavg`, `runtime.NumCPU` and `/proc/meminfo`; off Linux, or
   when they cannot be read, no cap). The machine has the last word,
   below min if need be.

The capacity in force moves toward the target **one step at a time, at
most once every 5 minutes** (`Step`, `StepInterval`), so one noisy
reading cannot swing it. **Shrinking only stops claiming**: a running
session is never stopped, and the node claims again once running falls
below the capacity. **At min, only items whose goal is `urgent` or
`high` are claimed.**

### In the loop

`Loop.scale` (`internal/node/scaling.go`) runs inside every tick but
evaluates at most once a minute (`ScaleEvery`) by the loop's own clock,
whatever the poll interval. Each evaluation records `Running() ×` the
minutes since the last as the slot-minutes run, feeds the windows as
read now (`Loop.Windows`, a `WindowSource`; by default the limits
store, taking the **highest utilization across accounts per window**),
and applies `Step` to the capacity in force, which `Loop.Capacity()`
returns. No reading at all sets no bound: max and the machine decide.

- A ranged node **starts at min**, and the start counts as a step: its
  first move is 5 minutes after it comes up.
- The min-only filter applies to **claiming items only** — plans,
  evaluations and rebases are not capacity-bound and are not filtered by
  it (pacing near the five-hour limit still filters plans). Below min
  (held there by the machine) is treated as at min.
- **Reload** of node.yaml takes a new range at once: the capacity in
  force is clamped into it on that tick, without waiting for a step; a
  change to a plain number drops the scaler and runs the fixed path.
- Each change is logged once with its reason (`capacity 2 → 3: max`,
  `capacity 3 → 2: clamped into 1–2`). `Loop.Scaling()` gives the
  capacity in force, min, max and the reason for the heartbeat's
  `scaling`; a fixed node has none.

### Reasons

Each target carries why it is what it is:

| reason | when |
| --- | --- |
| `max` | both windows afford max or more (or there is no reading) |
| `weekly pace` | the seven-day window sets it |
| `five-hour pace` | the five-hour window sets it (below the seven-day's) |
| `machine load` | the machine capped it at the running count |

### The heartbeat's `scaling`

`HeartbeatNode` gains an optional `scaling { capacity, min, max,
reason }`: the capacity in force, the range and the reason. It is sent
only when node.yaml's capacity is a range, the way `draining` is sent
only when set, so a platform without the field goes on accepting the
heartbeat of a fixed node. It is the field runsheet goal
`limits-on-the-console` shows on the console's Nodes page, beside
`limits`. On the wire (`gateway.Scaling`) it is camelCase JSON,
the numbers as integers and the reason as one of the four strings
above:

```json
"scaling": {"capacity": 3, "min": 1, "max": 4, "reason": "weekly pace"}
```

The platform's node record keeps the registered capacity only, so the
node also leaves its scaling in `~/.mesh/node.scaling` each tick
(`Loop.Scaled`, `RunState.WriteScaling`; removed when the capacity is
fixed or the process exits), and `mesh node status` shows its own
node's capacity as `in-force/min–max (reason)` — `2/1–4 (weekly
pace)` — and a fixed node's as the number, as before.

### Built on node-reads-its-limits, not beside it

The scaler reads the windows that goal's parser keeps — the latest
`rate_limit_event` per account, `Limits.Readings` in
`internal/node/limits.go` — and does not parse the event itself;
its `WindowReading` is a local shape the loop fills from that parser, so
the scaler builds and tests on its own. The hard stops stay that goal's:
at or above `limits.stop` of either window, or a `status` not `allowed`,
the node claims nothing whatever the scaler says, and above
`pace_five_hour` its half-capacity and urgent/high-only rules apply to
the capacity in force.

## Consequences

- A fixed `capacity: N` node is unchanged: same file, same claims, no
  `scaling` in its heartbeat.
- A ranged node starts near `min` (the default burn is cautious) and
  grows as its own measured burn shows room; a busy mini holds it where
  it is.
- The platform half: runsheet needs the optional `scaling` input on
  `heartbeatNode` and goal `limits-on-the-console` shows it; until it
  lands the node must not send the field (land the platform side first).
- **Pending amendment**: projectmesh-io/mesh-specs trd §1 (Node,
  `HeartbeatNode { …, scaling? }`) and §3 (Pacing by the plan's limits:
  capacity as a range and the rule above) need this same amendment once
  that repo can be reached from a goal's items.
