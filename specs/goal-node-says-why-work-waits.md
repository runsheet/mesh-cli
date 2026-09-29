---
title: Goal — The node tells the platform why it left ready work unclaimed: which accounts are paced, until when, and what it will still take
kind: plan
status: running
parent: prd
summary: The node's heartbeat carries `holding` — each account's status, reset and what the node still takes on it, the summary decision and the goals it passed over for headroom — sent only once runsheet's HeartbeatNodeInput takes the field; the console's node and goal pages are runsheet's half.
---

# Goal — The node tells the platform why it left ready work unclaimed: which accounts are paced, until when, and what it will still take

Mesh goal `node-says-why-work-waits`, running: items 1–3 merged (2026-09-28/29), this document is item 4.

## Statement

Chris, 2026-09-28: 'when items are blocked because of subscription limits, could we show something on the goals page.' That day both Claude accounts on the mini were paced (claude - shhemail: `allowed_warning` until 2026-10-03 03:00Z; mesh - max20 until 18:00Z), and every 30 s the node logged

```
accounts: no account has headroom for 2 plan(s); judged claude - shhemail, mesh - max20; left unclaimed until one has
limits: paced … only urgent and high goals are claimed and planned, at most 3 item(s) at once, and no research
```

None of that reached the platform. The console's goals page showed queued, planning and ready goals with no hint that the whole estate was waiting on account headroom, and Chris asked twice that day why goals were not moving. The heartbeat already carried every account's limits (goal `heartbeat-carries-every-account`), and GoalIndex has `waitingOn`/`waitingReason`/`waitingSince` (holds). What was missing was the node's *decision*: the node now sends it as data, not a log line.

## Acceptance

- **mesh-cli tests:** a paced account and an exhausted account produce the report. Holds: items 1 and 2.
- **Live:** the mini's heartbeat carries the holding report, and the platform's node page shows it. This waits on runsheet's half (see Still owed).

## Items

| Seq | Title | Status | Branch | PR | Cost | Model |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | Node: the holding report — gateway.Holding on the heartbeat, a pure HoldingReport from the accounts' readings, sent only when the schema takes it | merged | mesh/node-says-why-work-waits-1 | runsheet/mesh-cli#325 (103f2da) | $2.48 | sonnet |
| 2 | Node: each tick keeps which goals it passed over for headroom and why, and the next heartbeat carries the holding report | merged | mesh/node-says-why-work-waits-2 | runsheet/mesh-cli#329 (2da9c40) | $1.57 | opus |
| 3 | mesh-specs: the TRD's HeartbeatNode names `holding` — the node's holding decision per account and the goals left unclaimed | merged | mesh/node-says-why-work-waits-3 | runsheet/mesh-specs#88 (e0bac18) | $1.13 | haiku |
| 4 | Goal doc: node-says-why-work-waits — what items 1–3 landed, the mini's by-hand upgrade, and the platform half owed to runsheet | running | mesh/node-says-why-work-waits-4 | — | — | opus (planned haiku, sharpened) |

Each model is the one the platform served the item on. All three merged items were evaluated on opus. Item 3's first evaluation failed: its TRD paragraph gave `mode` the per-account statuses and made `takes.priorities` one string. The retry matched it to `gateway.Holding`.

## What landed

**Item 1 — `gateway.Holding` and `HoldingReport` (runsheet/mesh-cli#325).**

- `gateway.Holding`, `HoldingAccount`, `HoldingTakes` and `HoldingUnclaimed` (`internal/gateway/client.go`) are the wire shape. `HeartbeatExtras.Holding` carries them.
- `node.HoldingReport(now, accounts, readings, cfg, capacity, p, unclaimed)` (`internal/node/holding.go`) is a pure function from the accounts' readings and the node's pacing. It gives each account's status and reset, what the node still takes on it, and the summary.
- The node sends the report only when `takesHolding` finds `holding` on `HeartbeatNodeInput` by introspection. The answer is kept an hour, like `takesScaling`, and a failed probe counts as a no.
- Tests cover a paced account and an exhausted account.
- The ADR-005 amendment (2026-09-28, "the node reports its holding decision as data") was added, with the matching AGENTS.md lines.

**Item 2 — the loop's unclaimed list and the heartbeat (runsheet/mesh-cli#329).**

- `claimReady` and `planRequested` record a pass-over, once per goal and kind per pass, with a count. They record one in three cases:
  - no account has headroom;
  - the urgent-only reserve;
  - a ranged node sitting at its min.

  Capacity alone is never a pass-over.
- The reason is one sentence. It names the goal's priority and each account's standing and reset.
- At the end of each `Tick` the loop builds `HoldingReport` and keeps it on `l.holding`. The heartbeat goes out before the pass, so each heartbeat carries the *last* pass's report. The first heartbeat after a start carries none.
- A draining node sends the last report unchanged.
- The tests:
  - `TestHeartbeatCarriesHoldingOfPacedAndExhaustedAccounts`: mode `paced`, both goals unclaimed as `item` and `plan`, the item still ready, the goal still planning.
  - `TestHeartbeatHoldingWithHeadroomLeavesNothingUnclaimed`: `unclaimed: []`.
  - `TestHoldingLeavesOutTheUrgentItemAPacedNodeClaims`.
- The ADR-005 amendment gained a paragraph on what counts as passed over and on the report being the last pass's.

**Item 3 — the TRD paragraph (runsheet/mesh-specs#88).**

- `specs/trd.md` §1 Node: `HeartbeatNode { … holding? }`, dated 2026-09-28. Its paragraph names every field below and its values, and says the field is optional and sent only when set.
- It names runsheet goal `goals-page-shows-account-limits`, the contract fixture `specs/contracts/heartbeatNode.json` and runsheet ADR 026.
- §3 Pacing by the plan's limits says the decision is reported on the heartbeat as `holding`.

### The field

`HeartbeatNodeInput.holding`, optional:

```json
{
  "mode": "normal | paced | stopped",
  "summary": "paced: urgent and high only, 3 item(s), no research; 1 item(s) and 1 plan(s) left unclaimed",
  "until": "2026-09-28T18:00:00Z",
  "accounts": [
    {
      "account": "mesh - max20",
      "kind": "subscription | api_key",
      "status": "ok | paced | exhausted | unknown",
      "resetsAt": "2026-09-28T18:00:00Z",
      "takes": { "priorities": ["urgent", "high"], "maxItems": 3, "research": false }
    }
  ],
  "unclaimed": [
    { "goalId": "…", "kind": "item | plan", "count": 1, "reason": "normal priority; …" }
  ]
}
```

- `mode` is the node's own pacing. Only `accounts[].status` takes ok/paced/exhausted/unknown. An account is `unknown` when the node has never seen a reading for it.
- `until` is the earliest reset among the accounts that hold, and is omitted when none do. `resetsAt` is omitted when unknown.
- `accounts` and `unclaimed` are always lists, `[]` when empty.
- `summary` is one of:
  - `normal: up to N item(s)`
  - `paced: urgent and high only, N item(s), no research`
  - `stopped: nothing new until <reset>: <why>`
  - `no headroom: nothing claimed until <reset>`

  followed by `; N item(s) and M plan(s) left unclaimed` when anything was passed over.

## The log lines that stay

The report is added beside the log lines; it doesn't replace them. node.log still says:

- `limits: paced until <reset>: <why> — only urgent and high goals are claimed and planned, at most N item(s) at once, and no research`, once per crossing (`internal/node/loop.go`);
- `accounts: no account has headroom for <n item(s), n plan(s)>; judged <labels>; left unclaimed until one has`, one line per tick while work waits (`logWaiting`).

Nothing is logged about `holding` itself: not when it is built, sent or left off. A platform without the field gets no report and no log line.

## Live check on the mini

**Not done yet.** No PR and no node.log evidence shows the mini running a binary with #329, or a heartbeat carrying `holding`.

After merge, upgrade by hand as AGENTS.md says:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o mesh-linux ./cmd/mesh
scp mesh-linux mini:~/bin/mesh.new
ssh mini mesh node upgrade --binary ~/bin/mesh.new
```

The node swaps the binary (keeping `.prev`), restarts at once and adopts its running `claude -p` sessions. Then, on the mini:

- **`mesh node status`** prints the credential, `This machine: node running (pid N).`, and the org's nodes: status, running, capacity, and the new version on the `*` row. It does **not** print `holding`. The report exists only on the heartbeat, and so on the platform.
- **`~/.mesh/node.log`** has, while the accounts are paced, the two lines above as before. With no update pending, its first lines say `no update pending`.
- **The heartbeat's `holding`** reaches the platform only once runsheet's `HeartbeatNodeInput` has the field: runsheet goal `goals-page-shows-account-limits`, with the fixture `specs/contracts/heartbeatNode.json` per runsheet ADR 026. Until then `takesHolding` answers no, the node keeps the key off the heartbeat, and the platform sees nothing. No error or log line appears on either side.
- Once the field is deployed, the node picks it up within an hour, when the cached introspection expires, or at once on a restart. The check is then the platform's node record, or the console's node page once it renders the field. Deploying the node before the platform is safe, because the key stays off until the field exists.

## Still owed

These are runsheet's, not this repo's: runsheet goal **`goals-page-shows-account-limits`**.

- **`HeartbeatNodeInput.holding`**, the node record that keeps it, and the contract fixture `specs/contracts/heartbeatNode.json` (runsheet ADR 026), in the shape above.
- **The node page** shows each account's status, reset and takes, and the summary.
- **The goal rows** on the goals page: a queued, planning or ready goal in the last report's `unclaimed` shows that it waits and why (the `reason`), with the account's reset. GoalIndex's `waitingOn`/`waitingReason`/`waitingSince` is the likely home for this.
- **Then the goal's live acceptance:** the mini's heartbeat carries `holding` and the node page shows it.

Nothing further is owed in mesh-cli for this goal.

## Log

- 2026-09-28 · Chris asks for the goals page to show when items are blocked by subscription limits; both accounts on the mini are paced.
- 2026-09-29 · #325 (item 1) and #329 (item 2) merged to mesh-cli; mesh-specs#88 (item 3) merged after one failed evaluation.
- 2026-09-29 · this goal doc (item 4). No live check yet: waiting on runsheet's field.
