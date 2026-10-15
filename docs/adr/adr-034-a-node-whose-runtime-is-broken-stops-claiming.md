---
title: "ADR-034: A node whose runtime is broken stops claiming"
summary: A sandboxed node whose container runtime cannot start a session's pod — a session's *sandbox.RuntimeError or a failed Sandbox.Probe — latches runtime_broken (Loop.NoteRuntimeBroken), claims nothing of any kind while running sessions and adoption go on, heartbeats status runtime_broken with podman's words in reason when the gateway's NodeStatus enum lists it (else the words, prefixed, in reason alone), opens its holding summary and unclaimed rows with `runtime_broken: `, leaves node.runtime for mesh node status, and probes every RuntimeProbeEvery (5 minutes) until a pass clears it and logs `runtime healed`.
status: accepted
---

# ADR-034: A node whose runtime is broken stops claiming

2026-10-15, goal `a-runtime-that-cannot-start-a-session-stops-claiming`
(item 3). Builds on [ADR-007](adr-007-the-sandbox-on-the-node.md) (the
sandbox on the node; item 1's `Sandbox.Probe` and `*sandbox.RuntimeError`)
and [ADR-030](adr-030-an-environment-failure-names-its-kind.md) (an
environment failure names its kind; item 2's `runtime_broken` releases).
Follows the TRD §3 rule for a harness that cannot run: release, stop
claiming, say so on the heartbeat, probe until it heals.

## Context

On grapevine, 2026-10-15 23:05 → 2026-10-16 00:49 local, a root `podman`
run poisoned the node user's rootless storage. Every sandboxed session's
pod failed to start. Item 2 made each failure hand its work back as
`runtime_broken: <podman's words>` rather than a code failure, but the
node went on claiming: every tick claimed the next ready item, failed to
start its pod, and released it again. The platform saw a busy node, not
a broken one, and nobody was told.

## Decision

1. **The latch.** `Loop.NoteRuntimeBroken(err)` sets the node's runtime
   state, a `RuntimeState{status: runtime_broken, reason, since}`. The
   reason is podman's words (`RuntimeReason` without its prefix). Two
   things set it: the `RuntimeBroken` hooks of the Runner (builds and
   main validations), Evaluator, Planner and Rebaser, which
   `mesh node start` wires to the loop, and a failed probe. The first
   note logs `runtime broken: <words>; claiming nothing until a probe
   passes (every 5m0s)`. A later note updates the reason and keeps
   `since`.
2. **What it stops.** While the latch is set, `Tick` makes no claim and
   starts nothing new of any kind: items, evaluations, plans, rebases,
   research and main validations. This works like a drain for new
   starts, but the process runs on. The heartbeat, self-update,
   adoption and the sessions already running go on. What was handed
   back stays handed back, as item 2 made it: a build or rebase is
   released (`ReleaseWorkItem`, attempts unchanged), an evaluation is
   recorded as no verdict, a plan claim is released with no try
   counted, and a main validation is failed back to `requested`. Each
   carries the `runtime_broken: …` reason and a `WorkspaceReleaseHold`
   hold.
3. **The probe.** Every `RuntimeProbeEvery` (5 minutes, by the loop's
   clock `Loop.Now`) the loop calls `Loop.RuntimeProbe`, which is
   `Sandbox.Probe` when the node has a sandbox and nil otherwise. The
   first tick probes at once, so a node started on a broken podman
   never claims. Each probe is bounded by `RuntimeProbeTimeout`, a
   minute.
   - A failing probe sets the latch, even before any session has tried.
   - A passing probe clears the latch and logs `runtime healed: broken
     since <t> (<words>); claiming again, <n> runtime_broken hold(s)
     lifted`. It also drops the `runtime_broken` holds
     (`Declines.ForgetPrefix`), so the released work is claimed again
     on the same tick.

   The probe runs before the heartbeat, so the state reaches the
   platform within one probe interval and one tick, well inside ten
   minutes.
4. **The heartbeat.** `HeartbeatNodeInput.status` is a String, and
   the platform folds only the values it knows (trd §1 Node:
   `harness_unavailable`). The node sends `status: runtime_broken`,
   with podman's words in `reason`, only when the gateway takes it.
   `gateway.Client.TakesNodeStatus` decides this: it introspects the
   `NodeStatus` enum and keeps the answer for `ScalingProbeEvery`.
   Without that answer, the words ride `reason` alone, as
   `runtime_broken: <words>`, and `status` stays online. This is how
   the sandbox's limit-kill condition is reported. The node never sends
   a value the platform would refuse. The `gatewaytest` fake serves the
   enum from `Fake.NodeStatuses` and refuses any other status, as the
   platform does.
5. **The holding report.** The pass makes no claim. Instead it lists
   each served org's ready items and records one `Unclaimed` row per
   goal, whose reason is `runtime_broken: <words>`. The summary then
   opens `runtime_broken: <words>; nothing claimed until a probe passes;
   …`, followed by the ordinary judgement (`runtimeHolding`).
6. **The state on disk.** Every tick, `Loop.RuntimeChanged` is told
   the state. `mesh node start` writes it with `RunState.WriteRuntime`
   to `~/.mesh/node.runtime` (JSON `status`, `reason`, `since`), beside
   `node.standing`. A healthy or healed runtime removes the file, and
   so does `RunState.Stopped`. `RunState.Runtime` reads the file back
   for `mesh node status`, which a later item of the goal prints.

## Consequences

- A node with a broken runtime is idle and says why, instead of
  claiming and releasing in a loop. Its items read as an environment
  failure (`runtime_broken: …`), not as evaluating or failed.
- Healthy sandboxed nodes probe every 5 minutes. Each probe creates and
  removes one throwaway pod labelled `mesh.probe`.
- A node with `sandbox: off` has no probe and no pods, so it never
  latches.
- The platform half, which folds `runtime_broken` into a
  `needs_person` row the way it folds `harness_unavailable`, is
  runsheet's. Until its `NodeStatus` enum lists the value, the
  console sees the reason only.

Tests: `internal/node/runtime_broken_e2e_test.go`
(`TestRuntimeBroken…`, on `gatewaytest.Fake` and `sandbox.Fake`) and
`internal/gateway/release_test.go`'s `TestTakesNodeStatus`.
