---
summary: A session's cost cap is now the smallest of the goal's remaining budget, node.yaml's `budgets:` figure for its kind (build, evaluate, plan, rebase, research) and model tier (haiku, sonnet, opus) — sonnet build defaults to $4, haiku $2, opus $10 — and the kind's own flag; read live like the accounts and project budgets, logged with its source on the session's starting line. A build past 60% of its budget with nothing committed is handed back for one commit-and-finish turn, and the next attempt is told what it spent on.
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
A reload that changes a figure in force logs it, one line per kind and
tier, with the effective figures — defaults applied, not node.yaml's
raw entry: adding `build: {sonnet: 7}` logs `budget build/sonnet $4.00
→ $7.00` and removing it `budget build/sonnet $7.00 → $4.00`; a kind
with no default shows `none` (`budget evaluate/opus none → $5.00`), and
a project budget `project budget <project> <old> → <new>`
(`budgetChanges`, goal `node-yaml-takes-effect-or-says-why-not`).

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

## Hand-back

2026-09-28, item 3 of the same goal.

A cap alone stops a runaway at its budget with nothing to show for it:
two of the ledger's ten costliest sessions spent their whole budget and
landed nothing. So a build is handed back before its cap:

- **The running cost is priced on the node.** `claude -p` reports
  `total_cost_usd` only on its result event, so the harness prices each
  assistant turn's own usage (once per message id) at its model tier's
  list price — `listPrices` in `internal/harness/handback.go`, one table
  dated 2026-09-28 (Opus 5, Sonnet 5, Haiku 4.5; cache reads a tenth of
  input, cache writes a quarter more). A model it cannot place is priced
  as opus, so it reaches its mark early rather than never. The figure is
  for the mark alone; the cost reported is still the CLI's.
- **The mark is `Job.HandBackAt` × `Job.MaxBudgetUSD`.** The runner sets
  `harness.DefaultHandBackAt` (0.6) on every build; 0 is off, and every
  other kind leaves it 0. `Job.HasCommit` — the runner's `git rev-list
  --count origin/<base>..HEAD` in the workspace (`HasCommitSince`; a git
  that cannot answer counts as a commit) — is asked once, when the mark
  is first passed. A commit on the branch means the session runs on to
  its cap as before.
- **No commit stops the session and resumes it once.** The unit is
  stopped and `claude -p --resume <session>` started on the budget left
  (`MaxBudgetUSD` less the priced cost) with one turn,
  `HandBackPrompt`: commit what works with a message naming what is
  done and what is not, run the tests once, finish, start nothing new.
  The Result is both runs' (cost, turns, tokens, tools), the resumed
  run's text and verdict, with `HandedBack` true and `SpentOn` — one
  paragraph from item 1's figures: the first run's cost against the
  budget, its turns, input per turn (mean, max), its largest tool
  results by bytes (each tool once) and its most-called tools.
- **The finish is the one an empty build already takes.** A handed-back
  build that committed pushes and finishes as any other; one that still
  has no commit pushes an empty branch and `FinishWorkItem`s, and the
  platform's empty-finish route (goal `empties-go-back-to-the-builder`)
  sends it back to `ready`, attempts+1. No new failure path.
- **The next attempt is told.** The node notes `SpentOn` on the item
  (`ReportWorkItemProgress`, just before the finish) and keeps it in
  `~/.mesh/items/<item>.last-session` (`RunState.RecordLastSession`);
  the item's next claim on the node renders it in `BuildPrompt` under
  "The last session on this item" (`PromptInput.LastSession`): read
  less, commit earlier. A build that runs to its end without a
  hand-back clears the record.

**Claude on the host only.** A sandboxed session's HOME — where the
session a `--resume` needs is kept — goes with its pod, Codex has no
budget flag and no priced stream to watch, and an adopted session
(`Attach`) is followed as it stands: all three are capped by their
budget alone. The record lives on the node that ran the session; an
item's next claim on another node starts without it until the platform
carries it on the item.
