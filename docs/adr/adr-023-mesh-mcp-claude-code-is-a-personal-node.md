---
summary: `mesh mcp` is the client half of Mesh inside Claude Code — a stdio MCP server on the person's own credential that shares the channel's server, proxies the hosted reads and names every work tool from its first release; later items of the goal amend it.
status: accepted
---

# ADR-023: `mesh mcp` — Claude Code is a personal node

2026-10-03, goal `mesh-mcp-joins-an-item-from-claude-code` (item 1).
Numbered 023 beside `adr-023-the-planner-splits-a-broad-goal.md`, which
landed the same day; cite this one as "ADR-023, mesh mcp".

## Context

A person who wants to work one of a goal's items in their own Claude
Code session has no way in today: the node's sessions are `claude -p`
units it starts itself, and channel mode (`internal/channel`) is a
server the node's daemon serves to its own persistent session, pushing
events through Claude Code's experimental `claude/channel` capability
on the node's credential. The goal asks for the other direction: Claude
Code adds Mesh once (`claude mcp add mesh -- mesh mcp`) and, on the
person's own login, sees the goals, joins an item, works it in a
checkout made locally and reports through the same tools a node uses.

## Decision

1. **One stdio server, two users.** The JSON-RPC server, `Tool` and
   `ObjectSchema` move from `internal/channel/mcp.go` to
   `internal/meshmcp` (`server.go`); `internal/channel` keeps aliases.
   `Server.Channel` advertises `experimental.claude/channel` and is set
   by channel mode alone — `mesh mcp` is a plain tools server and
   never pushes.
2. **`mesh mcp` on the person's credential.** `internal/commands/mcpcmd.go`
   opens `platformClient` — `~/.mesh/token`, the Latchkey credential
   `mesh node login` saved (refreshed under the lock) or an `rs_*` token
   — and serves `meshmcp.NewServer` on stdin/stdout. Nothing else is
   written to stdout.
3. **The tool list is whole from the first release.** `mesh_goals`
   (the live goals of every org `myOrgs` names, and their open items,
   from the gateway's goal and item reads), `mesh_status` (the items
   this server holds), and `mesh_join`, `mesh_switch`, `mesh_progress`,
   `mesh_ask`, `mesh_finish`, `mesh_release`, which answer `not yet
   available` until the goal's later items fill them in, so Claude
   Code's tool list and a `/mesh` skill written against it do not
   change shape as they land.

## Consequences

- Channel mode is unchanged on the wire: its server still advertises
  the channel capability and its tools are the same.
- `mesh mcp` is the client half; the personal node it registers
  (ADR-015's owner form, kind personal, capacity 1), mesh_join's
  checkout on the item token, the pass-through work tools and pairing
  are later items of the goal, each amending this ADR.
- Usage of a `mesh mcp` session is the person's Claude Code's own; it
  goes to their personal subscription credential once the node is
  registered.
