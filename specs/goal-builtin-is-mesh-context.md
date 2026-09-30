---
title: Goal — The built-in context tool is called mesh-context everywhere an agent or a person sees it; prologue survives only as a dated alias
kind: plan
status: draft
parent: prd
summary: The platform's built-in tool row is mesh-context, but the node still attached it as prologue; this doc records every item and its PR, the mini's by-hand upgrade, and what stays with runsheet's console and platform repos.
---

# Goal — The built-in context tool is called mesh-context everywhere an agent or a person sees it; prologue survives only as a dated alias

## Statement

Chris, 2026-09-28: 'I see mesh-context, then I see prologue still listed in places.' prologue-becomes-mesh renamed the product and the platform's built-in tool row is `mesh-context` (policies.MeshContextURL, builtIn: true, served by itemTools), but the node still attaches its built-in MCP server under the name `prologue` (mesh-cli internal/tools/tools.go `Builtin = "prologue"`; the session command line shows `--mcp-config {"mcpServers":{"prologue":…}} --allowedTools mcp__prologue__*`), so agents call `mcp__prologue__*` tools, the node's log says prologue, and a console tool named mesh-context would not clash with the node's check while one named prologue would. The console's context pages still live at /orgs/<org>/prologue/<project>, the health tools are exposed under both `mesh_health_*` and `prologue_health_*`, and the gateway serves /prologue/v1 beside /mesh/v1.

Finish the rename where an agent or a person sees it: (1) mesh-cli: the built-in server is `mesh-context` (tools.Builtin), rendered as mcp__mesh-context__*; the node treats a platform row with builtIn: true as its own and never renders it twice; the log lines say mesh-context; the prompts/briefs that tell agents to use `mcp__prologue__*` say mesh-context — grep internal/harness prompts and mesh-specs; keep accepting `prologue` in any config the node reads for one release; (2) console: routes move to /orgs/<org>/context/<project> with the old path redirecting; nav says Context; (3) platform: `prologue_health_*` tool names and /prologue/v1 stay as aliases for a month and are marked deprecated in trd §6c, with the date they go. Amend mesh-specs trd §3 Tools (Prologue is built in → mesh-context is built in) and adr-006. Mini needs the by-hand upgrade after the node half merges.

## Acceptance

mesh-cli tests: a session's MCP config names mesh-context, allowedTools carries mcp__mesh-context__*, a console tool named prologue no longer clashes and one named mesh-context does; console tests: /orgs/acme/prologue/x redirects to /orgs/acme/context/x; the platform's trd marks the aliases deprecated with a date. Live: the next session on the mini shows mesh-context in its command line and the agents' transcripts call mcp__mesh-context__*.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | Node: the built-in server is mesh-context — tools.Builtin, MeshContextMCP, allowedTools mcp__mesh-context__*, log lines; a console tool named prologue no longer clashes and a builtIn row is never rendered twice | superseded (conflicting with main) | mesh/builtin-is-mesh-context-1 | https://github.com/runsheet/mesh-cli/pull/319 | $11.57 |
| 2 | Node: prompts, INDEX.md and the channel brief tell agents to use mesh_document/mesh_search on mesh-context; the context read goes to /mesh/v1 with /prologue/v1 as fallback | failed (budget) | mesh/builtin-is-mesh-context-2 | https://github.com/runsheet/mesh-cli/pull/257 | $6.63 |
| 3 | Design: TRD §3 says mesh-context is built in and /mesh/v1 is the context read; ADR 006 amended; the prologue aliases listed as deprecated with the date they go | failed (weekly limit) | — | — | $0.00 |
| 4 | Goal doc: builtin-is-mesh-context, with the mini's by-hand upgrade after the node half merges | pending, then replaced | — | — | $0.00 |
| 5 | Node: the built-in server is mesh-context — tools.Builtin, MeshContextMCP, allowedTools mcp__mesh-context__*; a console tool named prologue no longer clashes, one named mesh-context does; a builtIn row is never rendered twice | superseded (lint) | mesh/builtin-is-mesh-context-5 | https://github.com/runsheet/mesh-cli/pull/264 | $13.09 |
| 6 | Node: prompts, INDEX.md and the channel brief tell agents to use mesh_document/mesh_search on mesh-context; the context read goes to /mesh/v1 with /prologue/v1 as fallback | merged | mesh/builtin-is-mesh-context-6 | https://github.com/runsheet/mesh-cli/pull/258 | $4.92 |
| 7 | Design: TRD §3 and §6c say mesh-context is built in and the context read is /mesh/v1; ADR 006 amended; the prologue aliases listed as deprecated, going 2026-10-28 | merged | mesh/builtin-is-mesh-context-7 | https://github.com/runsheet/mesh-specs/pull/57 | $0.59 |
| 8 | Goal doc: builtin-is-mesh-context — the items, the mini's by-hand upgrade after the node half merges, and what stays with the platform | failed evaluation | mesh/builtin-is-mesh-context-8 | — | $2.64 |
| 9 | Node: the built-in server is mesh-context — tools.Builtin, MeshContextMCP, mcpServers key and --allowedTools mcp__mesh-context__*, Codex [mcp_servers.mesh-context], log lines and notes; a console tool named prologue no longer clashes, one named mesh-context does; the platform's builtIn row is never rendered twice; prologue accepted as an alias when reading older sessions' streams and config | merged | mesh/builtin-is-mesh-context-9 | https://github.com/runsheet/mesh-cli/pull/396 | $3.50 |

## After merge

mesh-cli has no deploy: build for linux/amd64 with `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o mesh-linux ./cmd/mesh`, `scp` it to the mini as `~/bin/mesh.new`, then `mesh node upgrade --binary ~/bin/mesh.new` (README.md's "Upgrading a node without ending sessions"): the binary is swapped (`.prev` kept), the node restarts at once and adopts its running sessions.

The sign it worked is the next session's command line showing `--mcp-config {"mcpServers":{"mesh-context":…}} --allowedTools mcp__mesh-context__*` and its transcript calling mcp__mesh-context__*. Sessions adopted from the older node keep saying prologue until they end.

## What this project did not build

The console's /orgs/<org>/prologue/<project> → /orgs/<org>/context/<project> redirect and the nav saying Context live in runsheet/console, and the `prologue_health_*` tool aliases and the gateway's /prologue/v1 live in runsheet/runsheet; neither is a repo of project runsheet/mesh (mesh-portal holds no prologue route), so the goal's console-test acceptance cannot be met here and each needs a goal in its own project before the aliases go on 2026-10-28 as mesh-specs trd §6c records.

## Log

- 2026-09-30 · item 10 · wrote this goal doc in the shape of specs/goal-tools-show-their-use.md — the Items table for seqs 1–9, the After merge and What this project did not build sections — replacing item 8's differently-shaped attempt at the same document.
- 2026-09-30 · item 10 · attempt 3: item 9's https://github.com/runsheet/mesh-cli/pull/396 confirmed merged (2026-09-29); main already carried this file from https://github.com/runsheet/mesh-cli/pull/320, so the branch rewrites it in place rather than adding it.
