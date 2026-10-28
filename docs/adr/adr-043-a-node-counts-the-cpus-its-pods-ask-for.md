---
summary: A sandboxed node's capacity counts the CPUs its pods ask for — every pod reserves its --cpus (its sidecars their services_cpus) against capacity.cpu_budget, 1.25 × the cores by default — so a claim is made only when its job's estimate fits, a validation's groups and shards start in waves within the budget, and a job bigger than the budget runs alone; the job-count capacity stays as the outer limit.
status: accepted
---

# ADR-043: A node counts the CPUs its pods ask for

2026-10-29. Amends ADR-004 (capacity scales with headroom) and ADR-013
(validation runs from a declared config, as amended for groups and
shards by #1643).

## Context

On 2026-10-28 dl360 (96 cores, v0.2.632, capacity max 16) ran four pull
request validations of runsheet at once. Each splits `integration` into
eight shards beside a unit and a checks group: about ten pods, each with
a Postgres sidecar and an 8-to-16-CPU sandbox. Load reached 690, CPU
pressure 92%, and every shard crawled at under one CPU. A validation
counted as one job against capacity while it ran ten pods.

## Decision

1. **A CPU budget.** node.yaml's `capacity.cpu_budget` (default 1.25 ×
   the host's cores) is the CPUs the node's pods may reserve at once.
   Every pod reserves its `--cpus`: a session's through the sandbox
   (`Sandbox.Reserve`, the agent's cpus at Open, the services' budget
   when a sidecar starts, both back at Close), a validation's through
   its job (`CPUJob.Pod`, agent and services at once, before the pod is
   made).
2. **Claiming.** The node claims a job only when its estimate fits
   beside what is committed — the pods open, each running job's hold
   or its pods if more, and the pods claimed sessions are expected to
   open (held `ExpectFor` until one opens) — and no pod waits for room.
   A session's estimate is one pod (cpus + services_cpus); a
   validation's is every group and shard its repo's validate.yml ran
   when last read on this node (`validate.Lanes` × a pod), else four
   pods. The one claim weighs the next job at the largest waiting
   validation's estimate when any waits (Mesh ranks validations first),
   else one pod. A job bigger than the whole budget fits only when
   nothing is committed, and then runs alone.
3. **Within a job.** A claimed validation holds its estimate from its
   claim to its end; once its validate.yml is read the hold is set to
   what it runs (lowered at once, raised only into what is free). Its
   pods draw on the hold first, within the budget; past the hold they
   wait in turn for room. So shards that would push the node over the
   budget start in waves, each still in its own pod with its own
   Postgres. A build's or rebase's validation, whose first lane runs in
   the session's pod, holds nothing past it and opens its other lanes
   as room comes.
4. **No deadlock.** Sessions can fill the budget while each waits on its
   own validation's shards. When the first pod in turn has waited
   `CPUValveAfter` (2 minutes) and no job's pod is open anywhere, it
   opens past the budget — one pod at a time, never more.
5. **Visibility.** The budget, what is reserved and by how many pods,
   what is committed and what waits are on the health line, the admin
   status and metrics (`mesh_node_cpu_*`), the heartbeat's `cpuBudget`
   (sent once the platform's HeartbeatNodeInput takes it), and logged
   once a minute while near the limit (90%, or a pod waiting).
6. **The job-count cap stays** as the outer limit; a node without a
   sandbox has no budget.

## Consequences

- A runsheet validation (ten pods) no longer runs beside three others
  on dl360: the next claim waits until its estimate fits, and its own
  shards start as many at a time as the budget leaves room for.
- A pod's `--cpus` is a ceiling, not use; the 1.25 default allows for
  that. A node that idles under the budget can raise `cpu_budget`.
- A per-suite `cpus:` (#1653) changes what a lane's pod asks for; the
  lane's reservation should follow it once it lands (today every lane
  reserves the sandbox's cpus).
- Sessions adopted from an earlier process are not counted until they
  end.
