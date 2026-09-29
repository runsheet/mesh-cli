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

## Attested validation on the mini (node-attests-its-validation)

A build now runs its repository's `.runsheet/validate.yml` before the
push and uploads the attestation (ADR-014). The mini picks this up only
by the by-hand upgrade after the merge — build for linux/amd64, `scp`
to `~/bin/mesh.new`, `mesh node upgrade --binary ~/bin/mesh.new` — or
with the next release. Until Podman is on the mini its sessions are
unsandboxed, so every attestation it uploads says `runnerImage:
host:<hostname>` and `sandboxed: false`.
