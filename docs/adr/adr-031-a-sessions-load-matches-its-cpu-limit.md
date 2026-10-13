---
title: "ADR-031: A session's load matches its CPU limit"
summary: A sandboxed session's container is told its --cpus limit (GOMAXPROCS, OMP_NUM_THREADS and MAKEFLAGS=-j<n>, ceil of --cpus), so its tools fan out to the limit and not to the host's cores; and the host back-off judges the node user's slice's CPU pressure (host.cpu_some_avg60_max, default 25%) in place of load per core when it is read, keeps a hot host hot down to host.quiet_fraction (0.8) of the threshold, and takes at most one slot per HostStepEvery (1 minute) — so a load that swings around the threshold is not a capacity that swings with it.
status: accepted
---

# ADR-031: A session's load matches its CPU limit

2026-10-13, goal `a-sessions-load-matches-its-cpu-limit` (items 1–3).
Amends [ADR-023](adr-023-the-node-backs-off-a-hot-host.md) (when the host
is hot, and how fast the back-off steps) and
[ADR-007](adr-007-the-sandbox-on-the-node.md) (what a session's container
is told of its limits).

## Context

On 2026-10-13 and 14 both nodes' node.log on the dl360 (96 cores, cgroup
v2) carry hundreds of `capacity N → M: host hot: load X/96 cores` lines.
The 1-minute load swung around 144 — ADR-023's `load_per_core_max` of 1.5
— all day; every tick over it took a slot, and every five quiet minutes
gave one back.

Two things made it so. A pod runs with `--cpus N`, but every tool inside
saw the host's 96 cores: Go started 96 Ps, `nproc`, `make` and test
runners fanned out as wide, and a session with a limit of four put dozens
of runnable threads on the run queue. The load average counts those
threads whether or not the cgroup lets them run, so it read a host far
busier than its sessions were starved. And the back-off judged that
number with no band and no pause: a reading a hair either side of the
threshold was a step each way.

## Decision

- **A container is told its limit** (`internal/sandbox/podman.go`,
  `LimitEnv`, item 1). From `--cpus` the agent and each sidecar get
  `GOMAXPROCS`, `OMP_NUM_THREADS` (GNU `nproc` honours it) and
  `MAKEFLAGS=-j<n>`, `n` the ceiling of `--cpus`, at least 1; none when no
  CPU limit is set. They ride `--env NAME=value` beside `LimitArgs`; a
  name the spec's own env sets is left as it is.
- **The slice's pressure is read** (`MachineReading.SliceCPUSomeAvg60`,
  item 2): `user-<uid>.slice`'s `cpu.pressure` `some avg60`, the share of
  the last minute some task of the node user's waited for CPU — what the
  sessions are actually short of.
- **Hot by pressure when it is read** (`MachineReading.Hot`): with the
  slice's pressure read, the host is hot when it is past
  `host.cpu_some_avg60_max` (default 25), the reason `host hot: cpu
  pressure 31%/25%`, and load per core is not looked at. Without it — a
  cgroup v1 host, a node outside a user slice, off Linux — load per core
  past `host.load_per_core_max` (1.5) judges, as before. The package
  temperature past `host.package_temp_max_c` stays, after either.
- **A quiet band** (`host.quiet_fraction`, default 0.8, at most 1): once
  hot, the host stays hot until the pressure (or, without PSI, the load
  per core) is below that fraction of its threshold — 20% for the default
  25. A fraction of the threshold rather than a fixed figure, so it moves
  with a threshold edited in node.yaml. The temperature has no band: it
  moves slowly and its threshold is already a margin.
- **One step a minute** (`HostStepEvery`): while hot, the back-off takes
  a slot at most once per minute, not every tick — a minute's average
  needs a minute to answer a step. Nothing new is claimed on any hot
  tick, as before; the climb back stays one slot per `HostRecoverEvery`
  (five quiet minutes).
- Both fields default, are refused negative naming the key, and are
  reloaded every tick like the rest of `host:` (`host: cpu_some_avg60_max
  25 → 30`, `host: quiet_fraction 0.8 → 0.6`).

## Consequences

- A session's tools run as wide as its limit, so its share of the load
  average is about its share of the CPU.
- On a host whose slice pressure is read, a busy run queue that starves
  nobody no longer moves capacity; `TestReplayDl360DayCapacityHoldsStill`
  replays a day like 2026-10-13 — load around 1.5 per core all day, the
  slice's pressure under 25% — and logs fewer than 20 capacity lines,
  while a forty-minute spell at 40% still backs off to the floor and
  climbs back.
- A host hot by a reading that flaps around the threshold is one hot
  spell, held at the floor until it falls through the band, rather than a
  capacity that rises and falls with it.
- ADR-029's overload test (`host.overload_per_core`,
  `host.overload_cpu_some_avg60`) is unchanged: it judges a starved
  session, not the capacity.
- The goal's own check is on the dl360: fewer than 20 capacity changes a
  day in node.log after the release.
