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
`Project.MaxItems`/`ItemCap` awaiting `ProjectIndex.maxItems`, below).
The unit tests construct `gateway.Goal{RepoID:
...}` directly, as the planner's own callers do, so the rule is fully
exercised node-side without the wire selection.

## Amendment 2026-10-03: the plan check's close-out rule (item 4)

**The check.** `closeOutAcceptance(specs)`
(`internal/node/plan_check.go`) flags an item whose title or acceptance
matches a close-out phrase — "reads as one design," "describes what
landed," "records that/what … landed/merged/shipped," an ADR's status
or frontmatter moved to accepted or superseded (either word order: "ADR
018 status accepted" and "the status of ADR-018 to accepted" both
match), "docs-only," "close-out" — **and** whose acceptance names no
code or test check: no `go test`, `go build`, `go vet`, a `_test.go`,
`.go`, `.ts` or `.sh` file, `npm test`, a `make` *command* (`make
test`, `make build`, …, never the English verb — "the docs make sense
together" names no check), or a backticked command other than `grep`,
`cat` or `ls` on a `.md` file — a bare backticked document filename
(`` `README.md` ``) is a reference, not a command, and never counts
(`hasCodeOrTestCheck`). Both conditions are lexical, like
`hostReadingAcceptance`; the evaluator's unverifiable-clause rule is
the backstop for what this check misses. An ADR whose status line
moves beside its own code or test check is never flagged — the clause
rides a real change, not a close-out of its own.

The flag: `item N (title): a docs-only close-out — its acceptance only
amends documents to describe landed work (<phrase>); docs land in the
pull request of the change they describe (AGENTS.md): fold the clause
into that item`. It is appended in `readPlan` beside
`hostReadingAcceptance`, so the re-ask carries it with
`planCloseOutRule` the same way a host reading carries `planHostRule`
(ADR-020). A second answer that still carries a close-out item is
handed in with the flag logged, as ADR-020's reading flags are; the
evaluator's rule is the backstop.

the-fleet-stalls-loudly's item 22 — "the landed branches read as one
design: README, ADR 017 and the TRD describe the stall report," an
acceptance of document sections alone — is the fixture this check is
built against, folded into item 19's own acceptance in the clean graph
(`internal/node/plan_closeout_test.go`).

## Amendment 2026-10-03: the gateway half — ProposeGoalSplit, TakesGoalSplit, SetGoalCheckpoints

Item 7. The rest of the gateway half named in the Decision's
Consequences — `ProjectIndex.maxItems` and `Project.ItemCap` — is a
separate, later item (`Project.MaxItems`/`ItemCap` stays unwritten on
this branch; `fields/ProjectIndex.txt` and `fields_gen.go` are
untouched here): the live gateway still does not serve
`ProjectIndex.maxItems`, and ADR-010's rule keeps it off the allow-list
until it does. This item lands the three methods rule (a) and rule (c)
need to hand their answers to the platform, plus the fake that lets
both be tested before the platform ships either:

- `gateway.GoalDraft{Slug, Title, Statement, Acceptance, RepoID}` is
  one of the `split` answer's drafts.
- `Client.ProposeGoalSplit(ctx, goalID, by string, drafts []GoalDraft,
  costCents int, usage *UsageReport) error` sends `proposeGoalSplit` on
  the goal with `drafts` (each slug, title, statement, acceptance,
  repoId), `costCents` and, when non-empty, `by`, through the same
  `mutateWithUsage` every costed mutation uses.
- `Client.TakesGoalSplit(ctx) bool` is the introspection probe —
  `{ __type(name: "Mutation") { fields { name } } }` read for
  `proposeGoalSplit`, cached in a `holdingProbe` for `ScalingProbeEvery`
  the same way `TakesPlanQuestions` and `takesAttestations` are:
  `ProposeGoalSplit` calls it first and, when false, sends nothing and
  returns `&Error{Unsupported: true}` for the caller to fall back on.
- `Client.SetGoalCheckpoints(ctx, goalID, by string, checkpoints
  []string) error` sends `setGoalCheckpoints` with `checkpoints` (an
  empty list, never nil) and `by`. `SetGoalCheckpointsInput` is already
  on the live schema, so this one needs no probe.

`gatewaytest/fake.go` gets a `GoalSplit bool`: false (the platform
today — goal `a-goal-fits-in-eight-items` in `runsheet/runsheet` is
still `queued`) leaves `proposeGoalSplit` out of `mutationType()` and
refuses a call to it the same way an absent
`recordValidationAttestation` is refused, `Cannot query field
"proposeGoalSplit" on type "Mutation".`; true lists it, and `apply`
records the drafts and cost on the goal row (`goal["splitDrafts"]`,
`goal["splitCostCents"]`) as well as on the generic `Calls` list every
mutation already lands on, so a test can read either.
`setGoalCheckpoints` is on the live mutation list unconditionally and
sets `goal["checkpoints"]`.

`internal/gateway/goalsplit_test.go` proves: `ProposeGoalSplit` against
a `GoalSplit: true` fake sends the drafts and cost and the fake
records them; `TakesGoalSplit` is false against a fake without
`GoalSplit` and true against one with it; `ProposeGoalSplit` against a
fake without it is `IsUnsupported` and sends nothing; and
`SetGoalCheckpoints` sends the list and the fake's goal row holds it.

## Amendment 2026-10-03: ItemCap defaults to 8 until the platform serves maxItems

Item 8. `gateway.Project` gains `MaxItems int` (json `maxItems`),
`const DefaultMaxItems = 8` and `Project.ItemCap()`: the project's own
`MaxItems` when it is above 0, else `DefaultMaxItems`. `ItemCap` is the
planner's cap for rule (a) — the node's size rule reads it, never
`MaxItems` directly — so a plan is held to eight items on every platform
today.

**`maxItems?` is not yet in `fields/ProjectIndex.txt`**, so `MaxItems`
is always 0 from a fetched project and `ItemCap` always 8. ADR-010's
rule is platform-first: the committed `schema.json` is refreshed only
from the live gateway, and the generator fails on an allow-listed field
that schema lacks. Introspected at `https://api.runsheet.dev/graphql`
on 2026-10-03, and again while building this item on 2026-10-04,
`ProjectIndex` has no `maxItems`. `maxItems?` joins
`fields/ProjectIndex.txt` in a follow-up once a `schema.json` refreshed
from the live gateway carries `ProjectIndex.maxItems` — the platform
goal `a-goal-fits-in-eight-items` (runsheet/runsheet) — and then no node
code changes: `ItemCap` already prefers the served value.
`internal/gateway/itemcap_test.go` proves the default, the project's own
cap, and `Client.Project` against the gatewaytest fake (which serves no
`maxItems`, as today's platform) answering `ItemCap() == 8`.

## Amendment 2026-10-04: the plan check's cut-over rule (item 5)

**The check.** `cutOverAcceptance(specs)`
(`internal/node/plan_check.go`) flags each item with a sentence of its
acceptance that waits on a person or production: "a/the
person/operator/owner/human confirms/approves/deletes/runs/flips," "in
production," "once a/the run/release/deploy," "a v\*/live/real run
reads/shows/mirrors," "the gate is open/closed," "STEP 0 reading,"
"after the platform/runsheet half lands." A `Measure:` item is exempt,
as for the host rule (ADR-020). The flag — `item N (title): its
acceptance waits on a person or production ("<sentence>", …), a gate no
builder can open; make it `precondition` on the item that needs it, or
a `pr` checkpoint on the goal` — is appended in `readPlan` beside
`closeOutAcceptance`, so the re-ask carries it with `planCutOverRule`.
A second answer still carrying one is handed in with the flag logged;
the evaluator's unverifiable-clause rule is the backstop.

**The brief line.** A planned item's `precondition` rides its brief's
first line, `precondition: <clause>` — after `atomic:`'s line when the
item has one (`gateway.BriefWithPrecondition`, `gateway.PreconditionOf`,
mirroring `BriefWithAtomic`/`AtomicReason`) — so the item's record
carries the gate on every platform. It is also sent as the item's
`precondition` field (`gateway.PlannedItem.Precondition`) only when the
gateway's `PlannedItemInput` takes it (`Client.TakesItemPrecondition`,
introspected and kept an hour like `questions`); the gatewaytest fake's
`ItemPrecondition` serves the field, and without it a `planGoal` item
carrying it is refused, as the platform is today.

**The pr checkpoint.** `"checkpoints"` beside `items`
(`harness.ParsePlanCheckpoints`) naming `pr`, on a goal whose
`Checkpoints` lacks it, makes `handIn` call `SetGoalCheckpoints(goal,
by, goal.Checkpoints + "pr")` before `PlanGoal`, logged `goal <slug>: pr
checkpoint set: a person approves each pull request`; a refusal is
logged and the graph still lands. Any other value is logged `plan
checkpoint "<value>" is not one the node sets (pr); dropped`.

release-tag-mirrors item 14 — the cleanup after the flip, its
acceptance gated on a STEP 0 reading and on the operator deleting the
secret, failed three times — is the fixture
(`internal/node/plan_cutover_test.go`): flagged, re-asked, and handed in
with the gate as the item's precondition and a `pr` checkpoint on the
goal.
