# mesh-cli

End-user CLI for Project Mesh.

This CLI should target `mesh-api` as its proper backend for Mesh data operations.

This repo exists to give operators a first-class way to work with Mesh data without abusing the gateway connector or stuffing privileged database credentials into random shells.

## Purpose

`mesh-cli` should become the standard interface for:
- creating and updating specs
- creating and updating Ralph Loop work items
- recording handoffs, reviews, and learnings
- querying work items, specs, projects, and gateway resolution
- supporting machine and agent workflows with auditable auth

## Why this exists

Project Mesh now has two distinct domains:

1. **Gateway/runtime integration**
   - handled by `mesh-connector`
   - talks to OpenClaw gateways
   - execution/runtime bridge

2. **Mesh application data operations**
   - specs
   - work items
   - reviews
   - learnings
   - operator workflow

Those data operations should not depend on the gateway connector.
They should go through `mesh-api`, the dedicated machine-authenticated Mesh application API.

## v1 goal

Build a small CLI and machine-auth layer for Mesh data operations.

### Initial command targets

- `mesh auth login`
- `mesh auth whoami`
- `mesh work-item create`
- `mesh work-item update`
- `mesh work-item handoff`
- `mesh work-item review`
- `mesh work-item learning`
- `mesh spec create`
- `mesh spec update`
- `mesh project list`
- `mesh gateway resolve`

## Auth direction

Recommended direction:
- end-user login/session for CLI use
- auditable human identity
- no direct Supabase secret-key usage in operator shells
- machine auth reserved for non-human services and agents through `mesh-api`

## Documents

- `docs/spec.md` — product + technical v1 spec
- `docs/rollout.md` — suggested rollout path

## Status

Fresh repo scaffolded on 2026-04-08.
