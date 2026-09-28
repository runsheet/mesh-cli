# Mesh CLI + Machine Auth v1 Spec

## Problem

Agents need to create and update Mesh data such as specs, work items, handoffs, reviews, and learnings.

Today, the available write paths are awkward:
- browser/UI writes require an interactive signed-in user
- direct privileged database access is unsafe to spread across agent shells
- `mesh-connector` should remain focused on gateway/runtime integration, not become the general write API for all product data

We need a first-class machine-authenticated interface for Mesh data operations.

## Goal

Create a new `mesh-cli` repo that becomes the standard end-user/operator interface for Mesh application data.

## Non-goals

- replacing `mesh-connector`
- embedding gateway transport logic into the CLI
- exposing raw service-role credentials to agents
- building full automation for every workflow in v1

## Primary users

- PM agents
- DevLead agents
- Builder agents
- human operators
- scripts/automation running in trusted environments

## Product requirements

### Auth
- end-user authenticated requests
- user-scoped permissions
- actor identity on all writes
- auditable actions

### Work item operations
- create work item
- update work item state/owner/priority/scope
- record handoff
- record review
- record learning

### Spec operations
- create spec
- update spec
- link spec to work item
- transition spec status

### Read/query operations
- list projects
- list work items
- fetch work item detail
- resolve gateway for a work item

## CLI shape

### Auth
- `mesh auth login`
- `mesh auth whoami`
- `mesh auth logout`

### Projects
- `mesh project list`

### Specs
- `mesh spec create`
- `mesh spec update`
- `mesh spec status`

### Work items
- `mesh work-item create` should feel natural for PM, DevLead, and Builder usage
- `mesh work-item update` should support a quick move-style form for common state transitions and a richer flag form for full edits
- `mesh work-item handoff` should read like a role-to-role handoff packet, with a simple summary-first shape and richer optional packet fields
- `mesh work-item review` should accept human-friendly decisions like approve and changes, while normalizing to the canonical backend contract
- `mesh work-item learning` should support a lightweight outcome-only form plus a fuller learning packet
- `mesh work-item get`
- `mesh work-item list`

### Routing
- `mesh gateway resolve`

### Tool results on a node

`tool_results.max_kb` in `~/.mesh/node.yaml` (default 16, `0` off) caps
what one tool result may put into a node session's context. A build
session on the host gets a Claude Code `PreToolUse` hook on `Bash` in
the settings the node renders beside its MCP config (trd §3 Tools):
`mesh tool-trim --max-kb N --hook` answers with `updatedInput` whose
command is `( <cmd>\n) 2>&1 | mesh tool-trim --max-kb N`, skipping a
command that already pipes to `tool-trim` and one run in the
background. `mesh tool-trim` (`harness.TrimToolResult`) returns output
under the cap unchanged and anything over it as the first 60% and the
last 40% of the cap, cut at line boundaries, with
`[mesh: trimmed N KB of M KB; run the command again with a narrower filter if the middle matters]`
between. The session's environment also carries `BASH_MAX_OUTPUT_LENGTH`
(the cap in characters) and `MAX_MCP_OUTPUT_TOKENS` (the cap / 4), so
MCP results and anything the hook misses are bounded by the CLI too. A
sandboxed session gets the environment only; channel-mode sessions get
neither.

## API direction

Recommended backend architecture:
- portal-side application API layer
- CLI talks to that API as an authenticated end user
- API performs application-level authorization and persistence

Not recommended:
- CLI talking directly to Supabase with secret-key credentials
- reusing `mesh-connector` as the generic app-write API
- treating CLI as a machine-secret client

## Data/actor model

Each write should record:
- actor type: human | machine | agent
- actor id
- timestamp
- target object id
- operation performed

## v1 rollout

1. scaffold repo
2. define auth model
3. define minimal API contract
4. implement `work-item create/update/handoff/review/learning`
5. implement `spec create/update/status`
6. migrate agents to use CLI for Mesh data writes

## Planner: model choice by evidence

The plan prompt shows the scorecard's per-repo, per-model first-pass
rate and cost per item as an "Evidence: which models worked here"
table (`harness.PlanInput.Evidence`, `[]harness.ModelEvidence`), read
from the platform's `scorecard` query before the planner session
starts (`gateway.Client.Scorecard`; a nil map, no error, on an older
platform without it). **A model whose first-pass on a repo is under
0.5 over five or more items is not chosen for that repo**; the planner
is told to prefer the model with the best first-pass at the lowest
cost per item, and that the goal docs in the context name models that
worked. The node's own check (`internal/node/plan_check.go`,
`modelAgainstEvidence`) flags an item whose model falls below the 0.5
threshold on its repo and routes the flag through the same re-ask as
an unverifiable acceptance (`PlanInput.Rejected`), with the reason
`model <m> has first-pass <x> on <repo>`.

## Usage report

Each session's usage report (`gateway.UsageReport`, carried as `usage`
on the command that ends it) also names its turn shape, added by goal
`mesh-spends-less-per-item`: `turns`, `inputPerTurnMean` and
`inputPerTurnMax` (its per-turn input tokens) and `largestToolResults`
(its five largest tool results by content byte length, `{tool,
bytes}`), all left off the report when zero or empty. See
`docs/adr/adr-005-accounts-on-the-node.md`'s "usage report per session"
section for the full contract.

## Success criteria

- end users can create/update Mesh data without direct database credentials
- connector remains focused on gateway integration
- writes are auditable and actor-attributed
- Ralph Loop operations become scriptable and repeatable
