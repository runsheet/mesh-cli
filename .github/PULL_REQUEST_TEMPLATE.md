## Summary

<!-- One paragraph: what changed, why. Reference any relevant
     `.planning/REQUIREMENTS.md` IDs (e.g. INV-02, DSP-04, MCP-06) and link
     to the spec or design doc if applicable. -->

## Compliance with PROJECT.md

<!-- REQUIRED. Quote the relevant Out-of-Scope bullet from
     `.planning/PROJECT.md` (or "n/a — does not touch any out-of-scope
     concern" with a one-line rationale). The reviewer will check this. -->

> _e.g._ "This PR wires a new `mesh_step_claim` MCP tool into the agent
> shim. PROJECT.md Out of Scope reads: 'Platform-owned third-party
> integrations (Discord, GitHub, Gmail, search APIs, etc.) — All
> third-party access lives in agent skills + native CLI tools.' This PR
> does NOT add an integration adapter; `mesh_step_claim` is part of the
> internal step-dispatch protocol between agents and mesh-api (Phase 3)."
>
> _OR_
>
> "n/a — this is an internal daemon refactor with no new platform-owned
> capabilities."

Reminder: `internal/gateways/discord/`, `internal/gateways/whatsapp/`,
`internal/channels/`, and `internal/accounts/` were archived in Phase 0
(see `.planning/phases/00-decommission-invariants/`). PRs that re-introduce
platform-owned channel gateways will be rejected per the load-bearing
constraint above; CI also asserts the directories remain archived.

## Test plan

- [ ] `go vet ./...` passes
- [ ] `go build ./...` passes (build the binary cleanly)
- [ ] `go test ./...` passes (full suite — Phase 0 sized small enough that
      the full suite is the right gate)
- [ ] `gofmt -l <touched files>` returns nothing
- [ ] Manual verification: <!-- e.g. `./mesh node start` with the change
      and exercise the affected command -->

<!-- See CONTRIBUTING.md for build/test guidance and the migration note
     for upgrading from pre-Phase-0 mesh-cli releases. -->
