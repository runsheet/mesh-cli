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

## `mesh_join`

Amended 2026-10-03 (item 3). `mesh_join(goal, item?)`
(`internal/meshmcp/join.go`, `Join`) resolves the goal by slug among
the orgs `myOrgs` names and the item by seq — the goal's first `ready`
item, lowest seq, when none is given — and claims it with
`claimWorkItem(itemId, <personal node>, "claude-code")`, the node's
path until the hosted `mesh_claim` (item 6). A refused claim is the
tool's answer, `claim refused: <reason>`, not a transport error. Then
it does what channel mode's `readyChannelItem` does:

- **Where the checkout lands.** `~/.mesh/work/<goal>/<seq>` on branch
  `mesh/<goal>-<seq>` from the item's base ref (`node.WorkspaceSpec`,
  `node.BranchName`, `node.PrepareWithRetry`), unless the cwd rule
  applies.
- **The cwd rule.** When the server's working directory is inside a git
  checkout whose `origin` names the item's repo (`OriginNames`: an
  https, ssh or path URL ending in `<owner>/<name>`, `.git` or not, any
  case), that checkout's top is the workspace instead: fetched, the
  branch checked out there, the context written under its
  `.mesh/context`. Its `origin` URL is left as the person set it.
- **The credential rule.** The item's own token (`issueItemRepoToken`,
  `node.NewItemCred`) goes on the checkout's own credential helper for
  GitHub (`node.WriteCredentialHelper`: in `.git/config`, an empty
  helper first, then the token's) — never the person's global config.
  A platform without item tokens falls back to the person's own `gh`
  and `git`, said once (`node.TokenFallback`), as a node's does.
- **What it answers.** After `node.FetchContext` (the item's repo, its
  goal, the checkout) and `node.WriteContext`: JSON `{path, branch,
  brief, acceptance, context: [files written], item: {id, goal, seq,
  title}}`. The held item is recorded in `~/.mesh/mcp-state.json`
  (`{held: [{itemId, goal, seq, path, branch, claimedAt}]}`), which
  `mesh_status` lists. A workspace that cannot be made releases the
  claim (`releaseWorkItem`) and answers the error.

The personal node the claim names is node.yaml's `node_id` until the
personal node's registration (item 2) answers its own.

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
