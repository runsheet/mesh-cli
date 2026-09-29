---
title: Goal — The node tells the platform why it left ready work unclaimed: which accounts are paced, until when, and what it will still take
kind: plan
status: running
parent: prd
summary: When items are blocked because of subscription limits, the node sends a holding report to the platform on every heartbeat showing each account's status, reset time, and current usage, so the console can show why work is waiting.
---

# Goal — The node tells the platform why it left ready work unclaimed: which accounts are paced, until when, and what it will still take

Mesh goal `node-says-why-work-waits`, running: items 1–3 merged (2026-09-29), this document item 4.

## Statement

2026-09-28 on the mini: both Claude accounts are paced (claude - shhemail: allowed_warning until 2026-10-03 03:00Z; mesh - max20 until 18:00Z) and the node logs, every 30 s, `'accounts: no account has headroom for 2 plan(s); judged claude - shhemail, mesh - max20; left unclaimed until one has'` and `'limits: paced … only urgent and high goals are claimed and planned, at most 3 item(s) at once, and no research'`. None of that reaches the platform: the console's goals page shows queued/planning/ready goals with no hint that the whole estate is waiting on account headroom, and Chris asked twice today why goals were not moving.

The heartbeat already carries every account's limits and status (goal `heartbeat-carries-every-account`). The node now sends the holding decision as data, not a log line: for each account, status (ok | paced | exhausted | unknown), the window's reset time, and what the node still takes on it (priorities, max items, research yes/no); and the summary decision — 'paced: urgent and high only, 3 items, no research' or 'no headroom: nothing claimed until <earliest reset>'. When the node passes over a specific ready item or plan because of headroom, the holding report tracks it, so the platform can show it on the goal.

## Acceptance

- **mesh-cli tests:** a paced account and an exhausted account produce the report; the loop records unclaimed goals and plans per pass; tests exercise the full shape.
- **Live:** the mini's heartbeat carries the holding report once runsheet's HeartbeatNodeInput has the field (runsheet goal `goals-page-shows-account-limits`); until then the node's introspection keeps the key off and the platform sees nothing.

## Items

| Seq | Title | Status | Branch | PR | Cost | Model |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | Gateway: the holding report — gateway.Holding on the heartbeat, a pure HoldingReport from the accounts' readings, sent only when the schema takes it | merged | mesh/node-says-why-work-waits-1 | runsheet/mesh-cli#325 (103f2da) | $2.30 | Sonnet 5 |
| 2 | Node: each tick keeps which goals it passed over for headroom and why, and the next heartbeat carries the holding report | merged | mesh/node-says-why-work-waits-2 | runsheet/mesh-cli#329 (2da9c40) | $3.85 | Sonnet 5 |
| 3 | mesh-specs: the TRD's HeartbeatNode names `holding` — the node's holding decision per account and the goals left unclaimed | merged | mesh/node-says-why-work-waits-3 | runsheet/mesh-specs#88 | — | — |
| 4 | Goal doc: node-says-why-work-waits — what items 1–3 landed, the mini's by-hand upgrade, and the platform half owed to runsheet | running | mesh/node-says-why-work-waits-4 | — | — | — |

## What landed

**Item 1: the holding report as data (#325).**

- **gateway.Holding** (`internal/gateway/client.go`) is the data shape with each account's status, reset time and usage ceiling. HeartbeatExtras now carries a Holding field.
- **HoldingReport** (`internal/node/holding.go`) is the pure function that builds the report from the node's readings and accounting — `HoldingReport(now, accounts, readings, cfg, capacity, p, unclaimed)` — producing each account's status (ok/paced/exhausted/unknown), its reset time, what the node still takes on it (priorities, max items, research yes/no), and the summary judgement ('paced: urgent and high only, 3 items, no research' or 'no headroom: nothing claimed until <reset>').
- **Introspection guards the field.** `takesHolding` (cached an hour) checks whether HeartbeatNodeInput in the schema has a holding field; sent only when it does.
- **Tests** exercise the report shape with paced and exhausted accounts, with and without headroom.
- **ADR-005 amended** with the shape, the rule and the conditions for sending.

**Item 2: the loop's unclaimed accounting (#329).**

- **claimReady and planRequested** record a pass-over once per goal and kind (no account with headroom, the urgent-only reserve, a ranged node at its min), with one reason naming the priority and each account's standing.
- **Tick builds HoldingReport** at the end of each pass and the next heartbeat carries it; a draining node sends the last one unchanged.
- **The 'passed over' entry** records which goals and plans the node left unclaimed that pass, their count, and the reason — one line per kind per tick, with no duplication when multiple reasons apply.
- **ADR-005 amended** (2026-09-28) with what counts as passed over and when the loop sends the report.

**Item 3: the TRD amendment (#88 on mesh-specs).**

- **HeartbeatNode** section names `holding` — the data structure, when it is sent, what each field means, and the conditions on sending (introspection, schema field present).
- The node's holding decision is the platform half's `goals-page-shows-account-limits` responsibility to render.

## Design

The holding report amendment is in `docs/adr/adr-005-accounts-on-the-node.md` ("Amendment: 2026-09-28 — the node reports its holding decision as data"). The mesh-specs side is item 3: the TRD's HeartbeatNode section in runsheet/mesh-specs#88.

## Live check on the mini

After item 3 merges and the platform's runsheet goal `goals-page-shows-account-limits` lands its HeartbeatNodeInput with the `holding` field, deploy by hand as described in the README's "Upgrading a node without ending sessions":

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o mesh-linux ./cmd/mesh
scp mesh-linux mini:~/bin/mesh.new
mesh node upgrade --binary ~/bin/mesh.new
```

Then check that the heartbeat carries the holding report:

```sh
mesh node status
```

The heartbeat's `holding` object should appear in the output, showing each account's status, reset time, and what the node still takes on it.

**Until runsheet's HeartbeatNodeInput includes the `holding` field:** the node's introspection keeps the key off and the platform never sees it. Once the field is added to the schema, the node detects it on its next heartbeat (or at start) and sends the report thereafter.

## Still owed to the platform

**runsheet: the console's node and goal pages.** The platform side of this goal is the runsheet goal `goals-page-shows-account-limits`:

- **The Node page** should show the node's holding decision: which accounts are paced/exhausted, their reset times, and the summary judgement.
- **The Goals page** should show, for each queued/planning/ready goal, whether the node left it unclaimed and why — 'no account has headroom', 'urgent-only reserve', etc.
- **Order:** per the rule that the platform side lands first, runsheet's HeartbeatNodeInput.holding field and both pages' rendering go in before the node can be deployed.

## After merge: the mini

Deploy by hand as described in the README's "Upgrading a node without ending sessions":

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o mesh-linux ./cmd/mesh
scp mesh-linux mini:~/bin/mesh.new
ssh mini mesh node upgrade --binary ~/bin/mesh.new
```

The new process adopts the sessions the old one left running and sends the holding report on its next heartbeat (if runsheet's field is in place).

## Log

- 2026-09-28 · Chris: 'both accounts paced and nothing visible on the goals page'
- 2026-09-29 · items 1 (#325), 2 (#329) merged to mesh-cli; item 3 (mesh-specs#88) merged
- 2026-09-29 · goal doc written
