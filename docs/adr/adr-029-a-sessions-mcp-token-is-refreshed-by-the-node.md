---
title: "ADR-029: A session's MCP token is refreshed by the node"
summary: Every session's mesh-context is a stdio relay to the gateway's /mcp — `mesh mcp-relay --url <gw>/mcp --token-file <path>` on the host, harness.RelayScript under node in a pod — whose bearer is a 0600 file in the session's own directory (`<workdir>.session/`, the pod's HOME, the CODEX_HOME, a temp dir for the channel session) that node.TokenFiles writes from gateway.Client.Bearer at start, rewrites atomically every minute the bearer has rotated, and removes when the session's end is handled; state.json names the file so an adopting node keeps it too; no session config carries a bearer, so a session outlives any one rotation of the node's machine credential.
status: accepted
---

# ADR-029: A session's MCP token is refreshed by the node

2026-10-14, goal `a-sessions-mcp-token-outlives-the-session` (items
1–4). Amends [ADR-006](adr-006-session-tools-rendered-by-the-node.md)
(a session's tools are rendered by the node, secrets in env only) and
[ADR-009](adr-009-sessions-outlive-the-node.md) (sessions outlive the
node). (Numbered 029 beside
[adr-029-a-session-the-node-gives-up-on-is-stopped](adr-029-a-session-the-node-gives-up-on-is-stopped.md),
as the goal's plan named it.)

## Context

Chris's watcher, 2026-10-11 05:20Z: ten sessions on both DL360 nodes in
a few hours logged `mesh-context: first call errored: MCP server
"mesh-context" rejected the Authorization header in its config`, and
Cloud Logging showed ~14 POST 401s on `/mcp` per 40 minutes. The node
wrote each session's mesh-context entry as `{type: http, url:
<gw>/mcp, headers: {Authorization: Bearer <token>}}` with the node's
own bearer of the moment — on a Latchkey machine credential, an access
token that rotates. Claude Code does not re-read its MCP config
mid-session, so a session that outlived the token's remaining life lost
every tool: `mesh_progress`, `mesh_ask`, `mesh_finish`, every read. A
build of two hours started late in a token's life was certain to.

## Decision

1. **The relay.** A session's mesh-context is a stdio MCP server that
   POSTs each JSON-RPC message to `/mcp` on a bearer it reads from a
   file *per request*: `mesh mcp-relay --url <gw>/mcp --token-file
   <path>` (`internal/mcprelay`, hidden subcommand, item 1) on the host
   and for Codex; in a pod, which has no mesh binary and no route to the
   host, `node <HOME>/mesh-context-relay.js <url> <path>`
   (`harness.RelayScript`, `sandboxRelayServer`, item 2), the node
   image carrying Node for claude. Both carry `Mcp-Session-Id`, relay a
   JSON or event-stream reply a message per line, re-read the file and
   retry once on a 401, re-initialise once on a session the server
   forgot (404, or 400 naming the session), and answer a missing or
   empty file as a JSON-RPC error naming it.
2. **The entry.** `node.MeshContextMCP(bin, gatewayURL)` is
   `harness.MeshContextRelay(bin, <gw>/mcp, "")` under the built-in's
   name, `mesh-context` — so `--allowedTools mcp__mesh-context__*`,
   `nodeFinishes`, `HostedFinishTools`, `toolBroken` and the tool
   reports hold. It names no file and carries no bearer;
   `meshContextMCP` in nodecmd.go returns it, on `meshBinary()`, only
   while `gw.Bearer` answers.
3. **The file is the session's.** The harness places it
   (`harness.PlaceMeshContext`, `placeMeshContext`): `<workdir>.session/
   mesh-context.token` for a host Claude session (after
   `openSessionDir`, which empties that directory), the pod's HOME for a
   sandboxed one — the mount the mcp.json already rides, with
   `RelayScript` written beside it — the CODEX_HOME for Codex
   (`codexHome.placeToken`, before `config.toml` renders the stdio
   table), and a temp dir of the process's own for the channel session
   (`channel.Session.Tokens`, per restart). `state.json`'s `tokenFile`
   names it. An entry that is not the relay — a person's http one, a
   test's — or a harness with no keeper runs as it came.
4. **The node keeps it fresh.** `node.TokenFiles{Token: gw.Bearer}` is
   every harness's `TokenKeeper` (`ClaudeCode.Tokens`, `Codex.Tokens`,
   `channel.Session.Tokens`; `sessionTokens` in nodecmd.go, set once
   the platform client is made). `Keep(path)` writes the bearer before
   the agent starts — 0600, a temp file renamed over the path, so a
   relay never reads half a token — and every `TokenRefreshEvery` (a
   minute, well inside the margin `auth.NeedsRefresh` refreshes the
   machine credential at) asks again and rewrites a file whose bearer
   changed. The release removes the file when the session's end is
   handled; a detach releases it without removing it.
5. **An adopted session's file is kept by its adopter.**
   `ClaudeCode.Attach` keeps the file its state names
   (`keepAdopted`) for as long as it follows the session, removing it
   at the end and leaving it on a detach (ADR-009's amendment). A Codex
   session still ends with its node, its home and file with it.
6. **A person's CLI keeps the http form.** `meshmcp.Hosted` (`mesh
   mcp`'s hosted work tools) reads the person's bearer per request
   itself and builds its endpoint from `node.MeshContextHTTP`; no
   session the node starts gets that form.

## Options rejected

- **The claim's item token on `/mcp`** (the goal's first option, the
  ADR 009 item token minted for the claim with the session's lifetime):
  the gateway's `/mcp` takes a person's or a node's bearer, not an item
  token, and teaching it to is a change to `runsheet/runsheet`'s
  gateway, outside this project's repos; a token minted for the
  session's lifetime is also a longer-lived secret on disk than a
  rotating one.
- **A per-session token the gateway accepts for the session's item**:
  the same gateway change, and a new credential kind to mint, scope and
  revoke.
- **An http proxy on the host fronting `/mcp`** with the node's current
  token: a pod cannot reach the host (`NetworkPolicy.Ruleset` refuses
  the host and the LAN), and a listening port is one more thing another
  local process could talk to on the node's bearer.
- **Rewriting the config and asking Claude Code to reconnect**: a
  print-mode session takes no `/mcp` command.

## Consequences

- A session outlives any rotation: the proof is
  `internal/node/tokenfiles_test.go`'s
  `TestSessionStarted50MinutesBeforeRotationFinishesAfterIt` — a
  session started 50 fake minutes before the node's token rotates, the
  old token then refused by `gatewaytest.Fake`'s `/mcp` (`SetToken`,
  `MCPRefused`), calls `mesh_progress` and `mesh_finish` through
  `mcprelay.Relay` after the rotation, the finish on the new bearer.
  `TestTokenFileRewrittenBeforeExpiryAndRemovedAtTheEnd`, the harness's
  `TestAdoptedSessionTokenFileIsRefreshed`, the rendering tests for
  Claude host and pod and Codex (`internal/harness/meshcontext_test.go`)
  and the channel's `TestSessionMeshContextRelaysOnAKeptTokenFile`
  prove the rest.
- The bearer is in a file the session can read for its life — an
  exception to ADR-006's secrets in the environment only (its
  amendment of this date): 0600, in the session's own directory, never
  on a command line or in a config, gone with the session.
- One more process per session: the relay. Its errors go to the
  session's stderr (`mesh mcp-relay: …`), never the node's log.
- The DL360's node.log reading — no `rejected the Authorization
  header` line in a day — and the gateway's `/mcp` 401 count falling to
  the handful from bad clients are read by
  `scripts/mcp-auth-reading.sh` (item 4, `docs/mcp-auth-reading.md`)
  once this ships.
