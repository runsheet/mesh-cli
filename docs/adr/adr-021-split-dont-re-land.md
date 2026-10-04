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

A refusal that repeats is not planned a third time: the node keeps each
goal's last refusal, and a second identical one (normalised) releases
the plan claim with the refusal's text as the reason
(`releaseGoalPlan`) and holds the goal until its row changes —
`adr-023-a-refused-plan-is-released-not-replanned.md` (ADR-023, a
refused plan is released, not re-planned).

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

## Amendment 2026-10-02: the plan check, the brief's `atomic:` line and the refusal (item 2)

**The check.** `sameAcceptanceAsFailed(specs, existing)`
(`internal/node/plan_check.go`) compares each new item's acceptance,
by `normaliseAcceptance` — lower-case, backticks and punctuation
stripped, whitespace collapsed — with every item of the goal that is
`failed`, `superseded`, or `cancelled` with a `lastError` of its own (a
dependent a re-plan cancelled carries none). An equal one is flagged
for the re-ask, beside `unverifiableAcceptance` and
`hostReadingAcceptance`: `item N (title): its acceptance is the same as
failed item M's (lineage R, attempt A of 3) — split it: partition the
acceptance into two or more items, one PR each, naming the
dependency`. R and A are the failed item's `lineageRootSeq` and
`lineageAttempt`, read from `WorkItemIndex` as optional fields
(ADR-010: an older gateway still answers); without them R is M and the
attempt is left out. The planner reads the goal's items once and keeps
them for the check; "The items so far" gets their `Acceptance`,
`LineageRootSeq` and `LineageAttempt`.

**`atomic` on the brief.** `planGoal` has no field for the reason yet,
so the node writes it as the brief's first line, `atomic: <reason>`
(`gateway.BriefWithAtomic`); the item's record, and so the goal,
carries it, and `gateway.AtomicReason(brief)` reads it back
(`WorkItem.Atomic`). An item with `atomic` set is not flagged against a
failed item whose brief has no `atomic:` line; against one that has it
— the exemption spent — it is flagged like any other. When the platform
takes `atomic` on the planned item, the field replaces the line.

**The refusal on the second answer.** adr-004's backstop hands a
still-flagged second answer in as it stands; a re-land is not handed
in. A second answer — or an adopted planner session's, which is not
asked again — that still recreates a failed item at its acceptance is
an error from `readPlan`, `plan would re-land failed item M (lineage
R) at the same acceptance; refused`, logged and counted like any
planner error (`MaxPlanAttempts`), and `PlanGoal` is not called: the
goal never gets a fourth re-land from this node.

## Amendment 2026-10-02 — the evaluator asks for the split at the second failure

Mesh goal `split-dont-re-land`, item 3.

**Decision.** After `EvaluateWorkItem` records a failed verdict
(`afterEvaluate`'s hand-in), the evaluator counts the item's failed
verdicts: the earlier attempts' records it already read for
`PriorAttempts` (ADR-020) whose notes carry no `VERDICT: PASS` and are
not a no-verdict's reason (`no VERDICT line: …`, `…; not a verdict` —
an unreached evaluation is not a failed one, runsheet ADR 035, and the
platform keeps those out of `evaluations`), plus this one
(`failedVerdicts`). **When the count is exactly 2** it calls
`RequestPlanReplacing(goalID, nodeID, itemID)` — `requestPlan` with
`RequestPlanInput.replaceItem` — and logs `item <label>: failed
evaluation 2; asking for a split (narrow re-plan) instead of a third
build` (`askSplit`). A count of 1 is the platform's rework (rung 1) and
3 is the platform's own narrow re-plan (runsheet ADR 023, rung 3): the
node asks at neither. The ask follows the verdict, never precedes it,
so the hand-in stands whatever the answer:

- a refusal (`IsRefusal`) is the platform's ladder going on — logged
  once per item (`Evaluator.Splits`) as `item <label>: split refused by
  the platform: <reason>; the platform's ladder continues`, not an
  error;
- a gateway whose `RequestPlanInput` has no `replaceItem` —
  introspected and kept an hour, as `scaling` (adr-005 §Heartbeat) — is
  sent nothing and nothing is logged (`gateway.ErrNoReplaceItem`).

**What is owed to the platform.** The `Goal` aggregate must accept a
node's `RequestPlan {by, replace_item}` for an item that is `ready`
after its second failed evaluation (today the narrow re-plan is asked
for a failed item at rung 3), record `replacing_item_id`, and supersede
the item's third build: the item is not served to a builder again while
the narrow re-plan is pending, and the planner's replacements take its
dependents (`GoalPlanned.rewired`). Until it does, the request is
refused and the ladder runs as before; the cost of an early node is a
refused call and a log line.

## Amendment 2026-10-02: the planner's narrow mode (item 4)

**The read.** `GoalIndex` serves `replacingItemId` and `narrowReplans`
(runsheet ADR 023's third rung); the node reads them as optional fields
(`Goal.ReplacingItemID`, `Goal.NarrowReplans`, `fields/GoalIndex.txt`'s
`?`, ADR-010): an older gateway refuses them once and every plan reads
as whole from then on.

**The narrow mode.** When `Goal.ReplacingItemID` is set, `Planner.Plan`
finds that item among the goal's items, reads its recorded evaluations
(`WorkItemPriorAttempts`) and fills `PlanInput.Replacing` — title,
acceptance, status, lineage — and `ReplacingClauses`, every attempt's
failed clauses, oldest first, so the prompt's `## Split this item`
shows what to partition. It logs `goal <slug>: narrow re-plan replacing
item <seq> (lineage R, attempt A of 3): asking for a split`. An id the
goal's items do not hold is said and planned as whole.

**The check.** On a narrow plan the answer must hold two or more items,
or exactly one carrying `atomic` with a reason — and that one only when
the replaced item's brief carried no `atomic:` line, the exemption once
per lineage. Otherwise `readPlan` flags `narrow re-plan of item M
(lineage R) must replace it with two or more items whose acceptance
partitions it, or one atomic item with a reason` (with, for a spent
exemption, that item M was already atomic) for the re-ask; a second
answer — or an adopted session's — that still fails it is refused like
a re-land, `narrow re-plan of item M (lineage R) answered N item(s) and
no split; refused`, and `PlanGoal` is not called. The same-acceptance
check runs beside it, so a split that recreates the failed item's
acceptance in one of its parts is still flagged.

**The hand-in.** Only the replacement items are sent: `PlanGoal`, while
the goal carries `replacingItemId`, supersedes only that item, keeps
every other and rewires its dependents onto the replacement (runsheet
ADR 023), so the node neither cancels nor recreates them.
