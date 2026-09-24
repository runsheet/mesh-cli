---
summary: The node reads an item's model, agent slug and evaluate model off WorkItemIndex, resolves a named agent's model project → org → default by AgentID, hands the planner's per-item model in on planGoal, and falls back without the fields on a platform older than items-carry-a-model.
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

Which session runs on which model is the rest of the goal and amends
this ADR when it lands.

## Consequences

- A node built after this runs unchanged against a platform before
  `items-carry-a-model`; the one refused read per process is the cost.
- The agent lookup is a fallback: the platform already folds the named
  agent's model into `WorkItemIndex.model` when the plan gave none.
- If the platform changes how `AgentID` is derived, the node's lookup
  finds nothing and falls back to node.yaml — quietly. The derivation is
  part of the contract.
