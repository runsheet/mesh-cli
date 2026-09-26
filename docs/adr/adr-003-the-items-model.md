---
summary: The node reads an item's model, agent slug and evaluate model off WorkItemIndex, resolves a named agent's model project → org → default by AgentID, hands the planner's per-item model in on planGoal, runs the planner on the strongest configured model, builds and rebases on the item's model (else its agent's, else node.yaml) and evaluates on the stronger of it and sonnet, and falls back without the fields on a platform older than items-carry-a-model.
status: accepted
---

# ADR-003: A node reads the model the platform gives each item

## Context

runsheet goal `items-carry-a-model` (runsheet trd §6a) puts the tier an
item is built on on the item: `PlannedItem.model` on `planGoal`, and
`model`, `evaluateModel` and `agentSlug` on `WorkItemIndex` (the plan's
model, else its named agent's, else unset). An `Agent` definition
(`AgentIndex`, trd §6e) carries its own `model`. Goal
`node-runs-the-items-model` is the node half. The node runs against
platforms of more than one age, and a read that names a field the
schema lacks is refused whole, so asking for the new fields must not
cost an older platform its items.

## Decision

- `gateway.WorkItem` carries `ProjectID`, `AgentSlug`, `Model` and
  `EvaluateModel`. Every item read (`ReadyWorkItems`, `WorkItem`,
  `OrgWorkItems`, `GoalWorkItems`, `RequestedRebases`) asks for them
  beside the base fields. A gateway that answers "Cannot query field"
  for one of them is an older platform: the client repeats the read
  with the base fields and asks for the base fields alone for the rest
  of its life. The items come back with the new fields empty, which
  means what it meant before: the node's own model.
- The platform names an item's agent by slug, not id. An agent's model
  is read by its aggregate id, `AgentID(org, project, slug)` — the
  platform's v5 derivation (`runsheet/platform/ids.AgentID`), copied in
  `internal/gateway/ids.go`. `WorkItem.AgentIDs` lists the ids to try,
  the project's definition, then the org's, then the platform's default,
  the order `agentFor` resolves in. `AgentModel(id)` returns "" (no
  error) for a definition that does not exist or is retired, and for a
  platform without agents or without their model.
- `PlannedItem.Model` is sent on `planGoal` only for an item that has
  one. A gateway that refuses the input field as unknown is handed the
  graph again without it.
- `internal/node/models.go` ranks tiers haiku < sonnet < opus (a full id
  by its family; "" and anything unknown below all three) so the node
  can pick the stronger of two, and never evaluate below the builder.
- The planner runs on `Models.PlanModel()`: node.yaml's `plan` (or
  `--model-plan`) when set explicitly, else the strongest of `default`,
  `build`, `evaluate`, `channel` and `research`, else `opus`. It
  chooses every other item's model, so it is never the weak link.
- The planner's graph carries `model` and `model_reason` per item
  (`harness.PlannedSpec`), chosen by the rubric in `PlanPrompt`:
  `haiku` for mechanical items, `sonnet` for a well-specified change in
  one package with tests to copy, `opus` for anything cross-cutting,
  concurrency, migrations, security, a failing item's retry, or when
  unsure. The node trims the model onto `PlannedItem.Model`; one that
  is neither an alias nor a `claude-*` id is dropped with a log line —
  advice, not an error — and the item falls back as if the plan had
  named none. node.log records each item's model and reason when the
  plan is handed in. `mesh goal plan` carries a `model` from its YAML.

### Which session runs on which model

- A build and a rebase run on `BuildModel(item, node)`: the item's
  `model`, else its agent's (`AgentIDs` in order, the first with a
  model), else the node's own (node.yaml `build` or `rebase`, else
  `default`; "" the CLI's). A rebase works on the builder's branch, so
  it takes the builder's choice. An agent lookup that fails is treated
  as no model: the node's own.
- The evaluator runs on `EvaluateModel(item, node)`: the strongest of
  the node's own evaluate model, the item's model (as above, without
  the node fallback), the platform's `evaluateModel` and `sonnet` — never
  below the builder. An item that names none of them (an older platform,
  an item without an agent, an agent without a model) evaluates on the
  node's own evaluate model exactly as before. Of two that tie, the
  node's own id wins, so an operator's `claude-opus-5-5` is not traded
  for the alias `opus`.
- The model is recorded where the item's history is read: node.log
  says `item <goal>/<seq>: build on <model>`, `evaluate on <model>` and
  `rebase on <model>`; the build's first progress note is `session
  started: <branch> on <model>` (a rebase's `rebase session started:
  <branch> onto <base> on <model>`); the evaluation's notes end
  `(evaluated on <model>)`.

## Consequences

- A node built after this runs unchanged against a platform before
  `items-carry-a-model`; the one refused read per process is the cost.
- The agent lookup is a fallback: the platform already folds the named
  agent's model into `WorkItemIndex.model` when the plan gave none.
- If the platform changes how `AgentID` is derived, the node's lookup
  finds nothing and falls back to node.yaml — quietly. The derivation is
  part of the contract.
