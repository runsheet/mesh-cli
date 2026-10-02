---
summary: A lineage's attempts shrink — after an item's second failed evaluation its next attempt is a split into two or more items whose acceptance partitions the original's, the node's plan check refuses a re-plan that recreates a failed item at the same acceptance naming its lineage, and `atomic: <reason>` exempts an item that cannot be halved once.
status: accepted
---

# ADR-021: Split, don't re-land

2026-10-02, goal `split-dont-re-land` (item 1). The planner's half of
runsheet ADR 023's ladder and its lineage amendment
(`a-re-land-keeps-its-dependents-and-a-lineage-has-a-cap`); the pattern
of a planner-half rule is this repo's ADR-020 (`planHostRule`), and an
acceptance stays what the evaluator can see (mesh-specs ADR 004).

## Context

The week's largest line was evaluation failure: 136 items failed
"evaluation failed 3 times" for $800, and 167 superseded items cost
$905 more. In `a-node-has-an-owner`, item 1 "Schema and policy: a Node
has an owner …" failed three evaluations on real defects; the narrow
re-plan recreated it as item 8 "Retry of item 1 …" at the same scope,
which failed three more, then item 14 "Re-land the owner model …", then
item 20 — $58 on one scope before items 27–30 landed it as four small
pull requests, each on its first pass.

The evaluator was right each time. What was missing is the rule that
the next attempt must be smaller: the builder got three tries at the
whole, and the planner re-planned the failed item with the evaluator's
text in the brief but the same acceptance. The platform now records a
lineage (`lineage_root_seq`, `lineage_attempt`) and caps it at three
attempts (runsheet ADR 023, amendments 2026-10-02); this ADR decides
what the planner does inside that cap.

## Decision

**A lineage's attempts shrink.** After an item's second failed
evaluation, its third attempt is not the builder again: it is a split.
The node asks for a narrow re-plan of that item (runsheet ADR 023's
`RequestPlan {replace_item}`) and the planner replaces it with two or
more items whose acceptance clauses partition the original's — every
clause lands in exactly one new item and none is dropped — each a pull
request one evaluation can check, the new items naming their
dependency on each other with `because`. The platform re-points the
failed item's dependents onto the replacement through the lineage, so
nothing downstream is cancelled or recreated.

**The prompt.** `PlanPrompt` says so in rule 3 and again in the re-ask
(`planSplitRule`, beside `planAcceptanceRule` and `planHostRule`). In
"The items so far" a failed, superseded or cancelled item is rendered
with its whole acceptance — so the planner can partition it — and,
when known, `lineage <root seq>, attempt <n> of 3` (`ExistingItem`'s
`Acceptance`, `LineageRootSeq`, `LineageAttempt`); a merged item's
acceptance is not shown. On a narrow re-plan `PlanInput.Replacing` and
`ReplacingClauses` (the clauses its evaluations failed, oldest first)
write a `## Split this item` section before the rules: the item's
title, acceptance and failed clauses, and the ask for two or more
partitioning items or one `atomic` item.

**The plan check.** The node's plan check refuses a plan whose new
item has the same acceptance, normalised (case, whitespace and
punctuation folded), as a failed item of the same goal, naming the
lineage — `item <seq> (<title>): the same acceptance as failed item
<n> (lineage <root>, attempt <k>); split it` — and the re-ask carries
`planSplitRule`. The re-ask is the last ask, as for ADR-020's flags.

**`atomic: <reason>`.** An item that cannot be halved — a schema change,
one migration — may carry `"atomic": "<reason>"` in the graph
(`PlannedSpec.Atomic`, kept by `ParsePlan`). It is exempt from the
split once: the plan check lets an atomic item through at the same
acceptance, and the reason is recorded on the goal. The second time an
atomic item fails it must be split; `atomic` is not honoured twice in
one lineage.

**What the node owes the platform.** The node hands the item's
`atomic` reason in with `planGoal` as a field on the planned item,
sent only when the gateway's input has it (introspected, like
`questions`), and it asks for the narrow re-plan — `replaceItem` —
after the item's second failed evaluation, so that it is accepted
before a third failure, rather than letting a third build run. The
platform half (the `atomic` field on `WorkItem`, the narrow request at
the second failure) lands first, per this repo's rule that the
platform's contract changes before the node reports it.

## Consequences

- A failing scope costs at most two builds before it is split, instead
  of three builds per re-land times three re-lands; the replay of
  `a-node-has-an-owner` 1 → 8 → 14 → 20 ends at the split.
- The planner sees the acceptance it must partition; without it the
  narrow re-plan could only guess at the original's clauses from the
  title and the evaluator's notes.
- The lineage cap (runsheet ADR 023 (b)) still bounds the worst case: a
  split that fails again is a person's to decide at the cap.
- `atomic` is an escape hatch, not a loophole: a reason is required,
  it is recorded on the goal for a person to read, and it holds once.
- This item lands the prompt, the `Atomic` field and the
  `ExistingItem`/`PlanInput` fields; the plan check, the request at the
  second failure and the `planGoal` field are later items of the goal.
