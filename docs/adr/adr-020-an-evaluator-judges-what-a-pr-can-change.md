---
summary: Acceptance names only what a pull request can change; a reading of the host (load, wall time, a ratio of durations, a p90 over a day) is judged by the evaluator on its mechanism and recorded as a `READING:` line beside the verdict, never failed for the host's number; and a verdict that fails a clause an earlier attempt failed on opens with `fails on the same clause as attempt N: <clause>`, so the narrow re-plan changes the clause or escalates rather than rebuilding.
status: accepted
---

# ADR-020: An evaluator judges what a PR can change

2026-10-02, goal `an-evaluator-judges-what-a-pr-can-change` (item 1).
Amends mesh-specs ADR 004 (a clause the evaluator cannot check is
unverifiable, not failed) and this repo's `adr-005-codex-second-opinion.md`
and `adr-011-evaluations-run-on-the-family-with-headroom.md`: the rule
holds for both families alike, because Claude and Codex evaluators share
`EvaluatePrompt`.

## Context

runsheet's goal `ci-fits-the-mini` asked for its e2e suite cut into three
shards whose durations sat "within 20% of each other", measured by a
sequential run on the mini. The item was built and failed five times —
items 5, 14, 17, 20 and 24, 2026-09-28/29 — on that one clause, measured
while the mini's load ran 30–65 on 18 cores. Rounds of 1.09, 1.13 and
1.19 max/min were failed: for a README that said 1.2, for a stale
timings file, for a round the evaluator could not reproduce under load.
The mechanism — the duration planner, disjoint shard lists over all 309
tests, the tests, the `-check` tools — passed every time. It merged only
when item 28's brief asked for the balance to be "proven by
planned-seconds balance": once the acceptance asked for what a PR can
prove.

The goal's own acceptance asked for "the mini's load p90 under 1.5 ×
cores". That is a reading of the host (it was 3.6 ×), and no pull
request can satisfy it.

Each retry's evaluator was given the earlier attempts' reviews, and each
failed on the same clause in new words; nothing in the record said the
clause, not the build, was what kept failing, so the narrow re-plan
rebuilt the same item.

## Decision

### The planner's half

An item's acceptance names only what a pull request can change: code,
tests, docs, a script that reproduces a reading, and the reading
recorded honestly. A measurement of the host — load, wall time, a p90
over a day — goes in the goal doc's After section, or in a separate
`Measure:` item that is never a merge gate. A clause the planner cannot
phrase that way is a question for the person (the goal waits on a
person), not an item.

`PlanPrompt` says so in rule 3 and again in the re-ask
(`planHostRule`, beside `planAcceptanceRule`), with ci-fits-the-mini's
two shapes as the example: "the three shards' durations within 20% of
each other" is a host reading, "proven by planned-seconds balance and
disjoint shard lists" is what a PR can prove.

**`Measure:` items.** A reading the goal wants taken is its own item
titled `Measure: …`, whose acceptance is that the script runs and its
reading is recorded — whatever the number. No other item depends on its
number, and its evaluator judges it by the host rule below.

**`questions`.** The planner's JSON carries, beside `items`,

    "questions": [{"clause": "…", "question": "…"}]

one per clause of the goal it could not phrase as what a PR can change
(the goal's own "the mini's load p90 under 1.5 × cores" is one).
`ParsePlanQuestions` reads them; `ParsePlan` reads the items as before.
They ride `planGoal` as `questions` only when the gateway's
`PlanGoalInput` has the field (`TakesPlanQuestions`, introspected and
kept an hour, as `failedClauses` is on `evaluateWorkItem`). Until then
each is logged once, `plan <goal>: question for the person: <clause> —
<question>`, and counted in the `planned, …` line; `planGoal` has no
note field to append them to, so node.log is where the person reads
them. **The platform half is owed**: runsheet stores the questions and
sets the goal `waiting_on: person` while any is open.

**The node's check** (`hostReadingAcceptance`, `internal/node/plan_check.go`,
lexical like adr-004's `unverifiableAcceptance`): an item whose
acceptance has, in one sentence, a reading — `load`, `p90`, `wall time`,
`duration(s)`, `seconds` — and a threshold — `under`, `below`,
`within N%`, `at most`, `≤`, `< N`, `× cores` — is flagged with the
phrase from the reading to its threshold, unless its title starts
`Measure:`. `readPlan` appends its flags to the re-ask's Rejected list
beside the unverifiable ones; the second answer is handed in as it
stands, its flags logged (adr-004 decision 3), and the evaluator's host
rule is the backstop for what the check misses. The fixtures are
ci-fits-the-mini's graph as planned and as item 28 had it
(`internal/node/testdata/plans/cifits_before.json`, `cifits_after.json`).

### The evaluator's half

`EvaluatePrompt` carries a host-state rule (`evaluateHostRule`, rule 6,
after the unverifiable rule) for every evaluator: a clause whose truth
depends on the host the evaluator runs on is judged on its mechanism —
the script or test that takes the reading exists, runs, and the branch
records its reading truthfully. The evaluator takes its own reading and
writes it above the verdict, one line per such clause:

    READING: <the clause, as the acceptance words it> || <what it measured, when, under what load>

(`ClauseReading`, read by `ParseReadings`), recorded with the verdict.
It fails such a clause only for what the branch can change — the script
missing, a recorded figure that contradicts the file it cites, a stale
timings file — never because the host's number is over the threshold.
The prompt gives ci-fits-the-mini as its example, and draws the line
inside it: no round fails on its max/min ratio, which is recorded as a
reading, but item 14 still fails on its stale `shard_times.json` — the
median of earlier sharded rounds where the brief asked for a fresh
whole-package run — because the branch can change that.

A retry's evaluator (one with `PriorAttempts`) is also told: when it
fails a clause an earlier attempt failed, the first line of its review
is

    fails on the same clause as attempt N: <clause>

(`SameClauseLine`). `SameClause` finds such a repeat — clauses equal
once case, whitespace, backticks and trailing punctuation are
normalised, or one containing the other — and `NoteSameClause` prepends
the line to the notes when the model left it out, so the record carries
it either way. Both lines are contract, like `FAILED:` and `VERDICT:`.

## Consequences

- A host reading no longer fails an item whose mechanism holds; the
  reading is still taken and kept, so a regression in the number is
  visible without blocking a merge on the machine's weather.
- A clause failed twice says so in the verdict's first line, and the
  narrow re-plan can change that clause or escalate to the person
  instead of spending another build on it.
- Containment matching can call two different clauses the same when one
  is a short phrase inside another; the cost is a re-plan that looks at
  the clause again, which is cheaper than a sixth build.
- The planner's check is lexical: "a diff under 200 lines" is no
  reading and passes, but a sentence that names seconds and a size
  limit together is flagged; the cost is one re-ask.
- The node records the readings and applies `NoteSameClause` when it
  hands the verdict in, and the evaluator's prompt is given the
  lineage's previous verdicts: later items of this goal. Until the
  platform stores readings they ride the notes.
