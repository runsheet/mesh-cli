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

- **Item 1** (failed): Node: the built-in server is mesh-context — `tools.Builtin`, `MeshContextMCP`, allowedTools `mcp__mesh-context__*`, log lines; a console tool named prologue no longer clashes and a `builtIn` row is never rendered twice — https://github.com/runsheet/mesh-cli/pull/319 (evaluation failed three times; redone as items 5 and 9).
- **Item 2** (failed): Node: prompts, INDEX.md and the channel brief tell agents to use `mesh_document`/`mesh_search` on mesh-context; the context read goes to `/mesh/v1` with `/prologue/v1` as fallback — https://github.com/runsheet/mesh-cli/pull/257 (the session ran out of budget mid merge-conflict resolution; redone as item 6).
- **Item 3** (failed): Design: TRD §3 says mesh-context is built in and /mesh/v1 is the context read; ADR 006 amended; the prologue aliases listed as deprecated with the date they go. Its session lost its claim mid-build (the platform read it back as ready, claimed by nobody), so it never reached an evaluation or a pull request. Item 7 is its retry.
- **Item 4** (cancelled): Goal doc: builtin-is-mesh-context, with the mini's by-hand upgrade after the node half merges.
- **Item 5** (merged): Node: the built-in server is mesh-context — `tools.Builtin`, `MeshContextMCP`, allowedTools `mcp__mesh-context__*`; a console tool named prologue no longer clashes, one named mesh-context does; a `builtIn` row is never rendered twice — https://github.com/runsheet/mesh-cli/pull/264.
- **Item 6** (merged): Node: prompts, INDEX.md and the channel brief tell agents to use `mesh_document`/`mesh_search` on mesh-context; the context read goes to `/mesh/v1` with `/prologue/v1` as the fallback until 2026-10-28 — https://github.com/runsheet/mesh-cli/pull/258.
- **Item 7** (merged): Design: TRD §3 says mesh-context is built in and the context read is `/mesh/v1`; ADR 006 amended; the prologue aliases listed as deprecated, going 2026-10-28 — https://github.com/runsheet/mesh-specs/pull/57.
- **Item 8** (merged): Goal doc: builtin-is-mesh-context — the items, the mini's by-hand upgrade after the node half merges, and what stays with the platform — https://github.com/runsheet/mesh-cli/pull/320 (superseded in place by this document, through items 20 and 17).
- **Item 9** (merged): Node: the built-in server is mesh-context — `tools.Builtin`, `MeshContextMCP`, the `mcpServers` key and `--allowedTools mcp__mesh-context__*`, Codex's `[mcp_servers.mesh-context]`, log lines and notes; a console tool named prologue no longer clashes, one named mesh-context does; the platform's `builtIn` row is never rendered twice — https://github.com/runsheet/mesh-cli/pull/396.
- **Item 10** (failed): Goal doc: builtin-is-mesh-context — every item and its PR, the mini's by-hand upgrade after the node half merges, and the console and platform halves that stay with runsheet. Its acceptance demanded an added file where this document already existed on main as one to modify; redone as item 13.
- **Item 11** (cancelled): Design: ADR 007, ADR 010, ADR 011 and the PRD name the built-in attachment mesh-context where they describe current behaviour, in dated amendment lines; Prologue stays only as the product's former name or the dated alias. Redone as item 16.
- **Item 12** (cancelled): Node: the harness inputs' `Prologue` field is `MeshContext`, and present-tense comments in internal/ call the context service mesh-context; prologue stays only where it names the dated alias. Redone as item 14.
- **Item 13** (cancelled): Goal doc: builtin-is-mesh-context rewritten from the record — every item 1–12 with its outcome and PR, the mini's by-hand upgrade after mesh-cli #396, and the console and platform halves that stay with runsheet. Redone as item 17.
- **Item 14** (failed): Node: the harness inputs' `Prologue` field is `MeshContext`, and present-tense comments in internal/ name mesh-context; prologue stays only where it names the dated alias. Its greps contradicted its own brief, and it failed three evaluations with no pull request of its own; redone as item 18, whose merge (https://github.com/runsheet/mesh-cli/pull/558) carries the `MeshContext` field.
- **Item 15** (merged): Node docs: the mesh-cli ADRs name the built-in mesh-context where they describe current behaviour, in dated amendment lines; prologue stays only as the dated alias or inside a quoted record — https://github.com/runsheet/mesh-cli/pull/546.
- **Item 16** (merged): Design: the PRD, ADR 007, ADR 010, ADR 002 and the README name the context layer mesh-context where they describe current behaviour, in dated amendment lines; Prologue stays only as the product's former name or the dated alias — https://github.com/runsheet/mesh-specs/pull/125.
- **Item 17** (cancelled, then reopened): Goal doc: builtin-is-mesh-context rewritten from the record — every item 1–17 with its outcome and PR, the mini on v0.2.210 already attaching mesh-context, and the console and platform halves that stay with runsheet. Cancelled 2026-10-02 for item 20, whose rewrite merged as #562; reopened the same day, its retry reconciles that rewrite with the record — item 14 with no pull request, item 20 merged, the mini's checks read live.
- **Item 18** (merged): Node: the harness inputs' `Prologue` field is `MeshContext`, and every present-tense comment in internal/ names mesh-context; prologue stays only in the dated alias, the `/prologue/v1` fallback and the quoted `prologue: not found` record — https://github.com/runsheet/mesh-cli/pull/558.
- **Item 19** (merged): Node docs: the mesh-cli ADRs name the built-in mesh-context where they describe current behaviour, each with a dated amendment heading; prologue stays only as the dated alias or inside a quoted record — https://github.com/runsheet/mesh-cli/pull/559.
- **Item 20** (merged): Goal doc: builtin-is-mesh-context rewritten from the record — items 1–20 with outcome and PR, the mini on v0.2.210 already attaching mesh-context, and the console and platform halves that stay with runsheet — https://github.com/runsheet/mesh-cli/pull/562 (d3e1267).

## What landed

**runsheet/mesh-cli** (main):

- `tools.Builtin` is `mesh-context` (internal/tools/tools.go); a session's `--mcp-config` names it under `mcpServers` and `--allowedTools` carries `mcp__mesh-context__*` (#264, #396).
- A Codex session's `config.toml` declares the built-in as `[mcp_servers.mesh-context]` (#396).
- The platform's `builtIn: true` row is the node's own and is rendered once, never twice; a console tool named `prologue` no longer clashes and one named `mesh-context` does (#264, #396).
- `prologue` is read as an alias only from sessions an older node started and this one adopted (`tools.IsBuiltin`), until 2026-10-28 (#396).
- The prompts, INDEX.md and the channel brief tell agents to use `mesh_document`/`mesh_search`; the context read goes to `/mesh/v1`, with `/prologue/v1` as the fallback on a 404 until 2026-10-28 (#258).
- The harness inputs' `Prologue` field is `MeshContext`, and present-tense comments in internal/ name mesh-context (#558).
- The mesh-cli ADRs name the built-in mesh-context where they describe current behaviour, each with a dated amendment heading (#546, #559).

**runsheet/mesh-specs** (main):

- TRD §3 Tools says mesh-context is built in and the context read is `/mesh/v1`; ADR 006 amended; the prologue aliases listed as deprecated, going 2026-10-28 (#57).
- The PRD, ADR 007, ADR 010, ADR 002 and the README name the context layer mesh-context in dated amendment lines (#125).

## The mini

No by-hand upgrade was needed: the node self-updated. The rename merged 2026-09-29 (mesh-cli#264, then #396), and a merge to main touching `cmd/`, `internal/`, `images/node/`, `go.mod` or `go.sum` is tagged and released automatically (mesh-cli ADR-002), so the mini took the release through its own self-update without anyone running `mesh node upgrade`. Its `~/.mesh/node.log` reads `self-update: v0.2.210 is up; update confirmed` at 2026-10-02 03:49:08, and `self-update: v0.2.211 is up; update confirmed` at 05:52:14 for the release after it. `mesh_health_nodes` on 2026-10-02 reported the mini (`Mini`, org runsheet) online at `v0.2.210`, and every one of its live sessions' `tools` lists a tool with slug `mesh-context`, never `prologue`.

The checks, read on the mini on 2026-10-02:

- The command line: a session's `--mcp-config` carries `"mcpServers":{"mesh-context":…}` (with a console tool served, `"mcpServers":{"context7":…,"mesh-context":…}`), checked with `ps -eo args | grep -o -- '--mcp-config [^ ]*'`.
- `--allowedTools mcp__mesh-context__*`: the live sessions show `--allowedTools mcp__context7__*,mcp__mesh-context__*`.
- A transcript calls `mcp__mesh-context__*`: item 17's own session's `output.jsonl` has a `tool_use` named `mcp__mesh-context__mesh_items`, and every session's `system` event lists `{"name":"mesh-context","status":"connected"}` among its `mcp_servers`. Checked with `grep -l 'mcp__mesh-context__' ~/.mesh/work/*/*.session/output.jsonl`.

## Left to the platform

These pieces are runsheet/runsheet's, deprecated in runsheet/runsheet's trd §6c, not this project's repos — mesh-portal has no such route, so this project's console clause is owed by a goal on runsheet/runsheet:

- **Console redirect:** /orgs/<org>/prologue/<project> → /orgs/<org>/context/<project> with nav item 'Context'
- **Health tool aliases:** `prologue_health_*` tool names stay as aliases serving the platform's mesh-context built-in
- **Gateway endpoint alias:** /prologue/v1 stays as an alias to /mesh/v1
- **Deprecation:** all three aliases are marked deprecated in trd §6c, with removal date 2026-10-28

Status tables live on the platform only; this doc records outcomes.

## Log

- 2026-09-28 · item 4 · cancelled: redone as item 8.
- 2026-09-28 · item 1 · mesh-cli#319 failed: evaluation failed three times; redone as items 5 and 9.
- 2026-09-28 · item 2 · mesh-cli#257 failed: the session ran out of budget mid merge-conflict resolution; redone as item 6.
- 2026-09-28 · item 3 · failed: the session lost its claim mid-build; retried as item 7.
- 2026-09-28 · item 7 · mesh-specs#57 merged (769c38d): trd §3 Tools and ADR 006 say mesh-context is built in; `prologue`, `prologue_*` and `/prologue/v1` are aliases until 2026-10-28.
- 2026-09-28 · item 6 · mesh-cli#258 merged (514de9f): prompts, INDEX.md and the channel brief name `mesh_document`/`mesh_search`; the context read is `/mesh/v1` with `/prologue/v1` as the fallback.
- 2026-09-29 · item 5 · mesh-cli#264 merged (9bf33f8): the built-in server is mesh-context; a console tool named prologue no longer clashes, one named mesh-context does.
- 2026-09-29 · item 8 · mesh-cli#320 merged (f725f19): the goal doc records items through 7; superseded in place by this document.
- 2026-09-29 · item 9 · mesh-cli#396 merged (e2e031f): the built-in server is mesh-context everywhere the node attaches it, including Codex's `[mcp_servers.mesh-context]`.
- 2026-09-30 · item 10 · failed: its acceptance demanded an added file where this document already existed on main; redone as item 13.
- 2026-10-01 · item 11 · cancelled: redone as item 16.
- 2026-10-01 · item 12 · cancelled: redone as item 14.
- 2026-10-01 · item 13 · cancelled: redone as item 17.
- 2026-10-02 · item 14 · failed after three evaluations, no pull request: its greps contradicted its own brief; redone as item 18.
- 2026-10-02 · item 15 · mesh-cli#546 merged (5230ffb): the mesh-cli ADRs name mesh-context as the built-in, each with a dated amendment heading; prologue stays only as the dated alias or inside a quoted record.
- 2026-10-02 · item 16 · mesh-specs#125 merged: the PRD, ADR 007, ADR 010, ADR 002 and the README name mesh-context as the context layer, in dated amendment lines.
- 2026-10-02 · item 18 · mesh-cli#558 merged (298cf88): every present-tense comment in internal/ names mesh-context; prologue stays only in the dated alias, the `/prologue/v1` fallback and the quoted `prologue: not found` record.
- 2026-10-02 · item 19 · mesh-cli#559 merged (9dd1301): the mesh-cli ADRs name mesh-context where they describe current behaviour, each with a dated amendment heading.
- 2026-10-02 · item 17 · cancelled: like item 10's, its acceptance demanded an added file where this document already existed; replaced by item 20.
- 2026-10-02 · item 20 · mesh-cli#562 merged (d3e1267): the goal doc rewritten from the record for items 1–20.
- 2026-10-02 · item 17 · reopened: the rewrite reconciled with the record — item 14 failed with no pull request, #558 is item 18's, item 20 merged; the mini confirmed v0.2.210 by self-update, its command lines carry `mcp__mesh-context__*` and its transcripts call `mcp__mesh-context__mesh_items`.
