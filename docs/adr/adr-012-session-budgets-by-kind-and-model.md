---
summary: A session's cost cap is now the smallest of the goal's remaining budget, node.yaml's `budgets:` figure for its kind (build, evaluate, plan, rebase, research) and model tier (haiku, sonnet, opus) — sonnet build defaults to $4, haiku $2, opus $10 — and the kind's own flag; read live like the accounts and project budgets, logged with its source on the session's starting line.
status: accepted
---

# ADR-012: Session budgets by kind and model

2026-09-28, goal `mesh-spends-less-per-item` (item 2).

## Context

The usage ledger since 2026-09-27 showed builds at 66% of spend ($273,
146 sessions): the ten largest held 38% of all build input, eight of
them sonnet, 24–36M input tokens each, $6.84–$9.57 — at or near the flat
$10 `--max-budget-usd` cap every kind shared regardless of the model it
ran on. Two of those ten ended back in `ready` with nothing landed.
Sonnet builds ran a median 3.8M input tokens (p90 24M) for a median
$1.49; opus builds ran 1.2M (p90 6.8M) for $1.03 — the cheaper model
was not cheaper in practice, because nothing capped it any tighter than
the model that actually needed the room. `Runner.Budget(goal)` capped a
build by the goal's own remaining budget and one flat `MaxBudgetUSD`;
the other four
kinds (evaluate, plan, rebase, research) each carried their own flat
`MaxBudgetUSD` with no tie to the goal at all. Nothing distinguished a
haiku session, which should need very little room, from an opus one,
which may need a great deal.

## Decision

**A budget is chosen per kind and per model tier.** `~/.mesh/node.yaml`'s
`budgets:` is `map[kind]map[tier]float64`, tier being `haiku`, `sonnet`
or `opus` (`ModelTier`, matched the way `ModelRank` is: a full id such as
`claude-sonnet-5` by its family name). A kind or tier the file leaves
out takes its default — only `build` has one, since that is where the
ledger showed spend running away: haiku $2, sonnet $4, opus $10. The
other four kinds keep their historical flat fallback (evaluate $3, plan
$5, rebase $5, research $5) until `budgets:` names one for them, or the
platform does.

**The session's cap is the smallest of three figures**
(`SessionBudget`): the goal's own remaining budget (`BudgetCents` less
`SpentCents`, when the goal names one — a research session has no
single goal, so this drops out), the kind/model figure above, and the
caller's own fallback (the kind's `--*-budget-usd` flag, or its
historical literal default when that flag is left at 0). Zero in any of
the three means "the other ones win", as `Runner.Budget` always worked;
generalizing it to three figures instead of two needed no new rule, just
`minPositive` over however many are given.

**`Runner.Budget(goal, kind, model) float64`** replaces the old
`Budget(goal) float64` (its only kind was ever "build"); `Evaluator`,
`Planner`, `Rebaser` and `Director` each carry the same
`kindModelBudget`/`budgetWithSource` pair, so the same rule runs
everywhere a session's `harness.Job.MaxBudgetUSD` is set. `Evaluator`
and `Rebaser` gained the `Cfg *Config` field `Runner`, `Planner` and
`Director` already had, since the budget now needs node.yaml either way.

**Read live like the accounts and project budgets.** `budgets:` rides
`Spending` (renamed doc: "node.yaml's accounts, project budgets and
per-kind, per-model session budgets") alongside `Accounts` and
`ProjectBudgets`, threaded through `Reloaded` and `Loop.reload` the same
way; `Loop.KindModelBudget(kind, model)` reads the last-reloaded map, or
`Cfg`'s own before the first reload. Every session-kind struct's new
`Budgets func(kind, model string) (amount float64, source string)`
field is nil in a test that builds one directly (falling back to its
own `Cfg`) and `loop.KindModelBudget` in the running node — the same
seam `Runtimes func() []string` already used for live tool runtimes.

**The platform's override is pending.** The brief asks for the claimed
item's or its project's own `budgets` field to win over node.yaml once
schema.json carries one; it does not yet (`schema.json` has no
`budgets` field on `WorkItemIndex` or `ProjectIndex`, only Goal's
existing `budgetCents`), so `Config.KindModelBudget`'s doc comment says
so and no `fields/` entry or `go generate` is needed until the platform
side lands. `Loop.KindModelBudget` and `Config.KindModelBudget` are
already the one seam the platform's read would replace.

**The chosen budget and its source are logged on the session's starting
line**: `item <goal>/<seq>: build on sonnet, budget $4.00 (the default
for build/sonnet)`, or `budget $1.50 (the goal's remaining budget)`,
`budget $6.00 (node.yaml budgets.build.sonnet)`, `budget $3.00 (the
session's own cap)`, `budget $0.00 (no cap)`.

## Consequences

- A runaway sonnet build now stops at $4 by default instead of $10 —
  the ten costliest sessions the ledger showed would have been cut
  roughly in half, before item 3's hand-back turn even fires at 60%.
- An opus build's default ($10) is unchanged: opus already ran cheaper
  in practice and sometimes needs the room a cross-cutting change takes.
- A goal with little budget left still binds tighter than any kind/model
  figure, as it always did for builds; now every kind respects it, not
  only builds.
- node.yaml operators who want every kind capped by tier, not only
  builds, set `budgets:` for evaluate, plan, rebase and research too;
  nothing changes for an operator who sets none.
- The platform override (item or project `budgets`) is still to land;
  this ADR is where that read plugs in.
