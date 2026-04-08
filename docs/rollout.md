# Mesh CLI rollout notes

## Immediate next steps

1. Decide where machine auth lives
   - portal backend extension
   - separate Mesh API service

2. Define token issuance model
   - org-level machine tokens
   - agent-scoped tokens later if needed

3. Implement minimal work-item writes first
   - create
   - update
   - handoff
   - review
   - learning

4. Implement spec operations
   - create
   - update
   - status

5. Migrate agent instructions
   - PM
   - DevLead
   - Builder

## Recommendation

Build the backend write API first, then layer the CLI on top.

That keeps the architecture clean:
- API = policy + auth + persistence
- CLI = operator/agent UX

## First dogfood target

Use `mesh-cli` to operate:
- `Agent Config Gitification v1`

That gives us a real internal ops item to prove the flow.
