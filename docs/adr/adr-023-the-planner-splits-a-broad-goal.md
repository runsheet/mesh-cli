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

## Amendment: the node's half of the repo rule — `otherRepoItems`

Item 6. `internal/node/plan_check.go` gains `otherRepoItems(specs,
itemRepo, goalRepo) []string`: each item whose mapped repo is not
`goalRepo`'s is flagged `item N (title): names repo <owner/name>, not
the goal's <owner/name> — a different goal: leave it out, or make it a
draft of a split`. `readPlan` appends the flags to the first answer's,
so the re-ask carries `planRepoRule` (already in every re-ask's rule 3)
beside them; a final answer — the re-ask's, or an adopted session's —
still holding one is refused outright, like a re-land
(`reLand.refusal`): `otherRepoRefusal` names the first such item's
repo, `plan names repo <owner/name> outside the goal's <owner/name>; a
different goal; refused`, and nothing is handed in.

The goal's own repo (`goalRepo` in `internal/node/plan.go`) is
`Goal.RepoID` among the project's repos when the platform names one,
else the project's one repo when `project.RepoIDs` has exactly one,
else neither — and then the rule does not apply (the check in
`readPlan` that an item names one of the project's repos at all, for
the planner's project-wide fetch, stands either way). `Planner.plan`
resolves it once, alongside `projectRepoMap`, and threads it through
`ask` and `readPlan`; `AdoptPlan` resolves it the same way for a
session this node did not start.

**`Goal.RepoID` carries no selected field today.** `GoalIndex.repoId`
is not yet in the platform's schema (confirmed by introspecting
`api.runsheet.dev/graphql` while building this item) — ADR-010's rule:
a field the platform has not shipped cannot be selected, and the
generator enforces it by failing when an allow-listed field is not in
the committed `schema.json`. So `fields/GoalIndex.txt` does not gain
`repoId?` yet, and `gateway.Goal.RepoID` is always `""` from a fetched
goal — `goalRepo` always falls back to the project's one-repo case
until the platform ships the field and a later change adds `repoId?`
to the allow-list and regenerates (the same pattern as
`Project.MaxItems`/`ItemCap` in this goal's item 2, awaiting
`ProjectIndex.maxItems`). The unit tests construct `gateway.Goal{RepoID:
...}` directly, as the planner's own callers do, so the rule is fully
exercised node-side without the wire selection.
