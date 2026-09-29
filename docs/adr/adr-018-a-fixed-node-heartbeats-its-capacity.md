---
summary: Every node heartbeats `scaling {capacity, min, max, reason, accounts}` whenever the gateway's HeartbeatNodeInput takes it — a fixed node too, on any number of accounts, as its capacity in force with min = max = capacity and reason `fixed` — so a capacity changed in node.yaml under a running node reaches the platform on the next heartbeat, not at the next registration.
status: accepted
---

# ADR-018: A fixed node heartbeats its capacity

2026-09-29, goal `node-yaml-takes-effect-or-says-why-not` (item 1).
Amends the heartbeat's `scaling` as adr-004 and
adr-005-capacity-across-accounts left it (`Loop.heartbeatScaling`,
`internal/node/scaling.go`).

## Context

The mini's node.yaml went from `capacity: 6` to `capacity: 8` at
06:17Z on 2026-09-29 under a node started at 04:00:32Z. The node took it
on its next tick — `Loop.reload` → `setRange` logged `capacity 6 → 8` at
06:17:09 — but at 13:30 the platform's `NodeIndex(mini)` still said 6,
and did until the node restarted at 14:24 and registered again.

The platform learns a node's capacity two ways. `registerNode` sets
`NodeIndex.capacity` — and only a start registers. The heartbeat's
optional `scaling` sets `NodeIndex.scaling_capacity` (runsheet
`scalingSeen`), which the console shows in place of `capacity` when
present (runsheet/mesh ADR-001, node capacity on the console). The node
sent `scaling` only for a ranged capacity or a fixed one on several
accounts; a fixed node on one account — the mini, and the ordinary case
— sent none, so a live change to its capacity had no way to reach the
platform.

## Decision

- `heartbeatScaling` sends `scaling` for every node. A fixed one sends
  its capacity in force as `capacity`, `min` and `max`, `reason: fixed`,
  and `accounts` as the list it already was (`[]` with no accounts
  scaler). A ranged node's `scaling` is unchanged.
- The gate is unchanged: `scaling` rides only when the gateway's
  `HeartbeatNodeInput` has it (`takesScaling`, introspected and kept an
  hour, like `commit` and `holding`); a platform without it gets the
  heartbeat without it. `reason: fixed` is already in the platform's
  vocabulary (the several-accounts case sends it), so the platform needs
  no change to accept it.
- What capacity means, the scaler and a ranged node's behaviour (adr-004)
  are untouched. `Loop.Scaling()`, `Loop.Scaled` and
  `~/.mesh/node.scaling` still describe a ranged node only.

## Consequences

- Within one heartbeat interval of a node.yaml capacity change the
  console shows the new capacity (`scaling.capacity`).
- `NodeIndex.capacity` itself — what `mesh_health_nodes` reports as
  `capacity` — is still the registered value until the next start. The
  platform should read `scaling_capacity` when set (or keep `capacity`
  in step with it); that is the platform's half, left with runsheet.
- A fixed node now pays the hour-cached `HeartbeatNodeInput`
  introspection that only ranged and multi-account nodes paid before.
