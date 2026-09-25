---
summary: A ranged node's capacity is the sum of what every account it may use can afford — per subscription, adr-004's windows rule on its own measured burn; per API key, its money left today (daily cap $50, monthly cap $1,000, the project's daily budget, list-price dollars) over the hours left today at its measured dollars per slot-hour, with the last 10% of the daily cap for urgent/high work and a hard stop at a cap — clamped to [min, max] and the machine, one step per 5 minutes; generalises ADR-004.
status: accepted
---

# ADR-005: Capacity across accounts

Numbered after mesh-specs ADR-005 (*A node's capacity is the sum of
what its accounts can afford*), which it implements on the node; this
repo's `adr-005-accounts-on-the-node.md` is the other half of the same
initiative and is unchanged.

## Context

ADR-004 made a node's capacity a range `{min, max}` that grows and
shrinks with one Claude subscription's windows. Since ADR-005 (accounts
on the node) a node runs on several accounts — several Max
subscriptions and API keys — and the initiative "Mesh runs anything, on
any account" (Chris, 2026-09-25) asks that nodes burst to what their
accounts can afford, that money is spent in order, and that API keys
never pass their caps. One subscription's headroom no longer describes
what the node can afford; the sum of its accounts does. Goal
`bursting-across-accounts`.

## Decision

### The rule

Every minute a ranged node computes a target
(`internal/node/capacity.go`, `AccountsScaler`), from readings, spend,
clock and machine all injected (`Observe`, `Target`), so the rule is
pure and tested with simulated accounts and a fake clock. Each account
keeps its own 30-minute history (`BurnWindow`).

1. **A subscription** affords, as ADR-004 counts it for one: the
   smaller over its windows of (target − utilization) ÷ minutes to
   reset ÷ its burn per slot-minute, with targets **0.90 five-hour and
   0.95 seven-day**, the burn measured over the last 30 minutes of *its
   own* sessions, the same default and floor burns before there is
   data. A window with no reading sets no bound. Reason: `five-hour
   pace` or `weekly pace` (or `max` when nothing bounds it).
2. **An API key** affords its money left today ÷ hours left today ÷
   its dollars per slot-hour. The money left is the smallest of:
   - the **daily cap** less spent today (default **$50**) — reason
     `daily cap`;
   - the **monthly cap** less spent this month (default **$1,000**) —
     reason `monthly cap`;
   - the **project's daily budget** less the project's spend today,
     shared by the project's keys in the order the node lists them —
     reason `project budget`.

   Dollars per slot-hour is the rise in the key's spend today over the
   slot-hours run on it in the last 30 minutes; before there is data,
   or across midnight, it is `DefaultDollarsPerSlotHour` ($10, cautious
   on purpose), and never below `FloorDollarsPerSlotHour` ($0.10). Days
   and months begin at midnight in the scaler's `Location` (UTC unless
   set).
3. **The reserve**: the last **10%** of a key's daily cap
   (`UrgentReserve`) is for `urgent`/`high` work only. A key's slots are
   `Slots{Normal, UrgentOnly}`: normal slots count only the money above
   the reserve; urgent-only slots are the rest. At 90% of its daily cap
   a key has no normal slots, only urgent-only ones.
4. **Hard stop**: a key at or over its daily or monthly cap affords
   nothing, whatever its rate.
5. **Capacity = the sum of every account's slots** (each rounded down,
   none above max), clamped to `[min, max]`, then capped by the machine
   as ADR-004 does: load average at or above the CPU count, or
   `MemAvailable` at or below 2 GB, holds it at the sessions running.
   `AccountsTarget.UrgentOnly` says how many of it only urgent/high work
   may use.
6. **One step per 5 minutes** (`Step`, unchanged); shrinking only stops
   claiming; at min only `urgent`/`high` items are claimed (ADR-004,
   unchanged).

### Reasons

The target's reason is `max` or `min` when it is clamped, `machine
load` when the machine holds it, else the reason every account shares
(`weekly pace`, `five-hour pace`, `daily cap`, `monthly cap`, `project
budget`), else `accounts` — the sum of several bounds. Each account
carries its own slots and reason (`AccountSlots`), for the heartbeat's
per-account slots.

### The spending order

Capacity counts what every account can afford; which account a session
runs on stays ADR-005's `ChooseAccount`: **subscriptions first; an API
key only for `urgent`/`high` work, or when every subscription is
exhausted and work waits.** An API key's normal slots widen the node
only under that order, and its urgent-only slots only for urgent/high
work.

In the loop (`internal/node/spending.go`, `Loop.spendable`) the
accounts `ChooseAccount` sees are first narrowed, for every session:

- an API key at or past its daily or monthly cap (by the ledger) is
  gone — the hard stop, whatever the priority;
- an API key inside the reserve (spent ≥ 90% of its daily cap) is gone
  unless the work is urgent/high;
- for an **item**, once the scaler has counted slots, an account whose
  slots its running items fill is gone: a subscription at its `Normal`
  slots, a key at its `Normal` (its total, for urgent/high work). A
  subscription with no slot free therefore counts as **exhausted**, so
  a normal item that waits moves onto a key's normal slots — the burst
  the capacity promised — and never onto its reserve.

The loop also keeps the capacity's `UrgentOnly` slots from normal and
low items: they take at most capacity − `UrgentOnly` − running.

### node.yaml and the ledger

node.yaml's `accounts:` (adr-005 accounts on the node) gain, for an api
account, `daily_cap`, `monthly_cap` (0 or absent: the defaults) and
`project`; the file gains `project_budgets: {project: dollars}`. They
hold names and caps only, never a secret, are refused when negative or
set on a subscription, and are read again every tick (`Reloaded.
Spending`). A node.yaml without accounts runs ADR-004's `Scaler` and
chooses as before.

Every one-shot session's list-price cost lands in the **spend ledger**
(`internal/node/ledger.go`, `ObserveSpend`) under the account its env
runs on: per account, today and this month in the node's **local
time**, with the slot-minutes run on it today, kept in
`~/.mesh/ledger.json` so a restart does not reset the caps. A project's
spend today is the sum of its keys'. The loop sets the scaler's
`Location` to the ledger's, so days and months begin at local midnight
(the scaler alone still defaults to UTC).

### Money

Every cap and budget counts **list-price dollars** — each session's
`total_cost_usd` as the CLI reports it — never a discounted or billed
amount, so a cap means the same on every key.

## Consequences

- ADR-004's `Scaler` stays as it is and is the one-subscription case:
  the same readings give the same target (tested). A fixed `capacity: N`
  node is unchanged.
- Two Max subscriptions afford roughly twice what one did; an API key
  widens the node by what its day's money buys and never past its caps;
  the last tenth of a key's day is held for the work that matters most.
- Money is bounded three ways — per key per day, per key per month, per
  project per day.
- A ranged node that lists accounts runs `AccountsScaler` in
  `Loop.scale` on each subscription's windows, each key's spend from the
  ledger and the slot-minutes run on each account; one without accounts
  runs ADR-004's `Scaler`. `Loop.AccountSlots` holds each account's
  slots; the heartbeat's per-account slots are a later item, and the
  platform half (runsheet accepting `scaling.accounts[]`) lands first.
