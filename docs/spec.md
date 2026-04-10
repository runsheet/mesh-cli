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
- `mesh work-item create`
- `mesh work-item update`
- `mesh work-item handoff`
- `mesh work-item review`
- `mesh work-item learning`
- `mesh work-item get`
- `mesh work-item list`

### Routing
- `mesh gateway resolve`

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

## Success criteria

- end users can create/update Mesh data without direct database credentials
- connector remains focused on gateway integration
- writes are auditable and actor-attributed
- Ralph Loop operations become scriptable and repeatable
