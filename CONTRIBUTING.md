# Contributing to mesh-cli

Mesh is a workspace of three sibling repos (`mesh-api`, `mesh-cli`,
`mesh-portal`) acting as one product. Cross-repo coordination guidance
lives at `/home/chris/workspace/mesh/CLAUDE.md`. Direction-of-product lives
at `.planning/PROJECT.md`.

## Compliance with PROJECT.md

Project Mesh has a load-bearing constraint:

> **The platform never owns third-party integrations.** Discord/GitHub/email/
> web search live in agent skills + native CLI tools on the operator's node.
> The platform never holds a third-party API key and never adds a third-party
> SDK import under `mesh-api/src/`. See `.planning/PROJECT.md` Out of Scope,
> top entry.

For mesh-cli specifically: `internal/gateways/discord/` and
`internal/gateways/whatsapp/` were archived in Phase 0 (`git rm`-ed; see
`.planning/phases/00-decommission-invariants/00-04-SUMMARY.md`). The
`internal/channels/` and `internal/accounts/` packages were also deleted as
part of Phase 0's full-cut. Do not re-introduce platform-owned integration
gateways in this repo. The Phase 2 dispatcher rebuild
(`.planning/REQUIREMENTS.md` DSP-01..07) will provide the in-process step
delivery mechanism that replaces them.

Every PR description must open with a `## Compliance with PROJECT.md`
section. The PR template at `.github/PULL_REQUEST_TEMPLATE.md` auto-fills
it.

## Building locally

```bash
cd mesh-cli
go vet ./...
go build ./cmd/mesh/   # produces ./mesh
go test ./...
```

## Running tests

```bash
go test ./...
```

There is no GitHub CI on pull requests. Every check (vet, build, the Go
tests, gofmt, the generated gateway fields, the script tests, and the
INV-02 archival checks) is a suite in `.runsheet/validate.yml`. The Mesh
node runs those suites on each change and attests the result before it
pushes (runsheet ADR 032). GitHub Actions only releases, on GitHub-hosted
runners. The repo is public, so the home box's self-hosted runners take
none of its jobs.

## Editing the proto contract

`proto/mesh.proto` is the source of truth for the wire contract between
mesh-cli and mesh-api. Changes require running `make gen` in `proto/` AND
regenerating the TS stubs in `mesh-api/src/grpc/proto/mesh.ts`. Both repos
must release together — there is no on-the-wire compatibility shim.

Before changing a shared shape (auth token, gRPC envelope, MCP tool schema,
workflow JSONB), grep all three repos for callers per
`/home/chris/workspace/mesh/CLAUDE.md`.

## Migrating from pre-Phase-0 mesh-cli

If you are upgrading from a pre-Phase-0 release that had `mesh discord`,
`mesh whatsapp`, and `mesh channel-server` commands:

1. Stop the daemon: `mesh node stop` (if running).
2. Purge stored third-party secrets — these were operator-side state the
   platform should never have held:
   ```bash
   rm -rf ~/.mesh/accounts/discord ~/.mesh/accounts/whatsapp
   ```
   (If you want to migrate the bots into agent-side skills, manually copy
   the bot tokens out FIRST. The platform's archival of the gateway code
   does not delete the on-disk tokens — that's a deliberate operator-action
   responsibility.)
3. Upgrade mesh-cli:
   ```bash
   # however you install: brew, manual download, etc.
   ```
4. Restart: `mesh node start`. The daemon no longer attempts to spawn
   gateway subprocesses; the channel-routing seat is empty pending the
   Phase 2 dispatcher (DSP-01..07).
5. Any operator-authored Claude Code skill that previously called
   `/discord/pair`, `/discord/list-channels`, `/whatsapp/pair`, etc. via
   the local socket gets 404s now. Move that capability into an agent-side
   skill that consumes Discord/WhatsApp via the operator's own native CLI
   (e.g. a `discord-cli` tool the agent invokes), per the load-bearing
   constraint.

## Cross-repo conventions

See `mesh-api/CONTRIBUTING.md` and `/home/chris/workspace/mesh/CLAUDE.md`
for HTTP envelope shapes, naming conventions, and shared-shape
coordination guidance.
