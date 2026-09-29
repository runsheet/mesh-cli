---
title: Goal — The built-in context tool is called mesh-context everywhere an agent or a person sees it; prologue survives only as a dated alias
kind: plan
status: draft
parent: prd
summary: The platform's built-in tool row is mesh-context, but the node still attaches it as prologue; finish the rename where an agent or person sees it — node, console, and platform aliases.
---

# Goal — The built-in context tool is called mesh-context everywhere an agent or a person sees it; prologue survives only as a dated alias

Mesh goal `builtin-is-mesh-context`, running.

## Statement

Chris, 2026-09-28: 'I see mesh-context, then I see prologue still listed in places.' The platform's built-in tool row is `mesh-context` (policies.MeshContextURL, builtIn: true, served by itemTools), but the node still attaches its built-in MCP server under the name `prologue` (mesh-cli internal/tools/tools.go `Builtin = "prologue"`; the session command line shows `--mcp-config {"mcpServers":{"prologue":…}} --allowedTools mcp__prologue__*`), so agents call `mcp__prologue__*` tools, the node's log says prologue, and a console tool named mesh-context would not clash with the node's check while one named prologue would. The console's context pages still live at /orgs/<org>/prologue/<project>, the health tools are exposed under both `mesh_health_*` and `prologue_health_*`, and the gateway serves /prologue/v1 beside /mesh/v1.

Finish the rename where an agent or a person sees it: (1) mesh-cli: the built-in server is `mesh-context` (tools.Builtin), rendered as mcp__mesh-context__*; the node treats a platform row with builtIn: true as its own and never renders it twice; the log lines say mesh-context; the prompts/briefs that tell agents to use `mcp__prologue__*` say mesh-context — grep internal/harness prompts and mesh-specs; keep accepting `prologue` in any config the node reads for one release; (2) console: routes move to /orgs/<org>/context/<project> with the old path redirecting; nav says Context; (3) platform: `prologue_health_*` tool names and /prologue/v1 stay as aliases for a month and are marked deprecated in trd §6c, with the date they go. Amend mesh-specs trd §3 Tools (Prologue is built in → mesh-context is built in) and adr-006. Mini needs the by-hand upgrade after the node half merges.

## Acceptance

mesh-cli tests: a session's MCP config names mesh-context, allowedTools carries mcp__mesh-context__*, a console tool named prologue no longer clashes and one named mesh-context does; console tests: /orgs/acme/prologue/x redirects to /orgs/acme/context/x; the platform's trd marks the aliases deprecated with a date. Live: the next session on the mini shows mesh-context in its command line and the agents' transcripts call mcp__mesh-context__*.

## Items

- **Item 3** (failed): Design: TRD §3 says mesh-context is built in and /mesh/v1 is the context read; ADR 006 amended; the prologue aliases listed as deprecated with the date they go. Its session lost its claim mid-build (the platform read it back as ready, claimed by nobody), so it never reached an evaluation or a pull request. Item 7 is its retry.
- **Item 5**: Node: the built-in server is mesh-context — `tools.Builtin`, `MeshContextMCP`, allowedTools `mcp__mesh-context__*`; a console tool named prologue no longer clashes, one named mesh-context does; a `builtIn` row is never rendered twice — https://github.com/runsheet/mesh-cli/pull/264 (evaluation passed; not merged into main when this doc was written, and being rebased onto main).
- **Item 6**: Node: prompts, INDEX.md and the channel brief tell agents to use `mesh_document`/`mesh_search` on mesh-context; the context read goes to `/mesh/v1` with `/prologue/v1` as the fallback until 2026-10-28 — https://github.com/runsheet/mesh-cli/pull/258 (merged, 514de9f).
- **Item 7**: Design: TRD §3 says mesh-context is built in and the context read is `/mesh/v1`; ADR 006 amended; the prologue aliases listed as deprecated, going 2026-10-28 — https://github.com/runsheet/mesh-specs/pull/57 (merged, 769c38d).

## Upgrading the mini

After item 5 (mesh-cli#264) merges, the node's binary must be upgraded by hand — mesh-cli has no deploy, so releases go out as signed GitHub artifacts only.

### Build and upgrade steps

```sh
# On the mesh-cli repo, after item 5 merges to origin/main
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o mesh-linux ./cmd/mesh
# Copy to the mini
scp mesh-linux mini:~/bin/mesh.new
# On the mini: swap the binary (.prev kept) and restart `mesh node start` at once;
# the new process adopts the running sessions
mesh node upgrade --binary ~/bin/mesh.new
# Sessions adopted across the restart keep their `prologue` MCP name and are accepted until 2026-10-28
```

### Verification

After upgrade, start a new session and check:

1. The session's command line shows `"mcpServers":{"mesh-context":…}` (not `prologue`)
2. The command line shows `--allowedTools mcp__mesh-context__*` (not `prologue`)
3. The session's transcript calls `mcp__mesh-context__*` tools (not `prologue`)

Example grep:
```
grep 'mcp__mesh-context__' output.jsonl | head -5
```

## Left to the platform

These pieces are runsheet/runsheet's (its trd §6c), not this project's repos — mesh-portal has no such routes:

- **Console redirect:** /orgs/<org>/prologue/<project> → /orgs/<org>/context/<project> with nav item 'Context'
- **Health tool aliases:** `prologue_health_*` tool names stay as aliases serving the platform's mesh-context built-in
- **Gateway endpoint alias:** /prologue/v1 stays as an alias to /mesh/v1
- **Deprecation:** all three aliases are marked deprecated in trd §6c, with removal date 2026-10-28

Status tables live on the platform only; this doc records outcomes.

## Log

- 2026-09-28 · item 3 · failed: the session lost its claim mid-build; retried as item 7.
- 2026-09-28 · item 7 · mesh-specs#57 merged (769c38d): trd §3 Tools and ADR 006 say mesh-context is built in; `prologue`, `prologue_*` and `/prologue/v1` are aliases until 2026-10-28.
- 2026-09-28 · item 6 · mesh-cli#258 merged (514de9f): prompts, INDEX.md and the channel brief name `mesh_document`/`mesh_search`; the context read is `/mesh/v1` with `/prologue/v1` as the fallback.
- 2026-09-28 · item 5 · mesh-cli#264 passed evaluation; not merged into main when this doc was written.
