---
summary: The planner is told four rules — a plan over the project's maxItems is answered `split` as goal drafts instead of items, a docs-only close-out item is refused and folded into the item it describes, a clause waiting on a person or production becomes a `precondition` or a `pr` checkpoint instead of an item, and an item naming a repo other than the goal's is a different goal.
status: accepted
---

# ADR-023: The planner splits a broad goal

2026-10-03, goal `the-planner-splits-a-broad-goal` (item 1). The
planner's half of `a-goal-fits-in-eight-items`; the pattern of a
planner-half rule beside the node's own check is this repo's ADR-020
(`planHostRule`) and ADR-021 (`planSplitRule`).

## Context

The planner writes a whole design as one graph, and the graph is
routinely too broad to finish: 46 items for `a-node-has-an-owner`, 41
for `rigger-sets-up-a-business`. `the-fleet-stalls-loudly` planned items
22, 25 and 28 as "the landed branches read as one design" — each
re-landing the same observation about the others' work, none of them a
pull request an evaluator could check against anything but a feeling.
`release-tag-mirrors` item 14 asked for a person to confirm the release
in production; no builder could open that gate, and it failed three
evaluations before a person noticed the clause was never an item's to
satisfy. And a plan can still name a repo the goal never claimed,
because nothing stopped it. The node's plan check
(`internal/node/plan_check.go`) had a rule for none of these: no size
rule, no close-out rule, no cut-over rule, no repo rule.

## Decision

Four rules, each written for the planner beside `planHostRule` and
`planSplitRule`, in rule 3 of every plan and again in the re-ask.

### (a) Size — `planSizeRule`

A plan with more than the project's `maxItems` (default eight) is not
sent as items. Told the cap is exceeded, the planner answers `split`
instead: goal drafts, each `{"slug", "title", "statement",
"acceptance", "repo"}`, each at most `maxItems` items and one repo, and
the node proposes them as goals (`ProposeGoalSplit`). The size rule
itself is Chris's, 2026-10-03: goals are too broad to finish, so one
repo, at most eight items, one pull request per item, and every
acceptance clause names how it is checked.

`PlanInput.MaxItems` carries the cap into rule 2 ("Two to `<MaxItems>`
items"); `PlanInput.OverCap` turns the re-ask's final sentence from
"write the whole graph again" into an ask for `split`, naming the cap.
`GoalDraft` is one such draft and `ParsePlanSplit` reads `{"split":
[...]}` from the same candidate scan `ParsePlan` uses
(`planCandidates`), so a `split` answer with no `items` is read, not
treated as a parse error.

### (b) Close-out — `planCloseOutRule`

An item whose acceptance only amends documents to describe landed
work — "read as one design," "describe what landed," amending an
ADR's status alone — is refused: docs land in the pull request of the
change they describe, per this repo's own `AGENTS.md`. The planner
folds such a clause into the item whose change it describes instead of
giving it an item of its own.

### (c) Cut-over — `planCutOverRule`

A clause that waits on a person's confirmation or a production
observation is never an item, because no builder can open that gate —
`release-tag-mirrors` item 14 is the rule's example, failed three times
on exactly this. It becomes `"precondition": "<clause>"` on the item
that depends on it (`PlannedSpec.Precondition`, kept by `ParsePlan`
like `atomic`), or, when the wait belongs to the goal rather than one
item, a `pr` checkpoint on the goal, written beside `items` as
`"checkpoints": ["pr"]` (`ParsePlanCheckpoints`, reading the same way
`ParsePlanQuestions` does).

### (d) Repo — `planRepoRule`

An item naming a repo other than the goal's repo (or the project's one
repo) is a different goal: the planner leaves it out, or makes it a
draft of a `split` answer naming that repo.

## Consequences

- A plan that would be 46 items now comes back as goal drafts a person
  can look at before anything is claimed, rather than as 46 items a
  node runs through regardless of whether they cohere.
- A close-out item that only restates what already landed is caught
  before a session spends a build on prose nobody asked for separately
  from the change.
- A cut-over clause stops costing three failed evaluations against a
  gate no builder could ever open; it rides the item that depends on it
  or the goal's own checkpoint instead.
- A repo the goal never claimed stops picking up work that belongs to a
  different goal's lineage and budget.
- This item lands the prompt, the four constants, `PlannedSpec.Precondition`,
  `GoalDraft`, `ParsePlanSplit` and `ParsePlanCheckpoints`. The plan
  check that enforces these rules (`internal/node/plan_check.go`), the
  gateway half (`ProjectIndex.maxItems`, `ProposeGoalSplit`,
  `SetGoalCheckpoints`) and the node's hand-in of a `split` answer are
  later items of this goal and amend it.
