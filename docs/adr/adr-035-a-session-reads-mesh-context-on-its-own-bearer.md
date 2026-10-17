---
title: "ADR-035: A session reads mesh-context on its own bearer"
summary: A build session's runsheet token file holds the id token of its own mesh session — opened by the node with POST /mesh/v1/sessions (item, claim seq, node id; runsheet ADR 056) and minted at the session's request URL for /mcp's audience — never the node's machine bearer, which reads every served org's MCP data; the file sits under the session's org's MCP root, its bearer is cleared and the file removed when the session ends, the claim-time read names its node, and a platform without the route refuses a node serving several orgs (the item released, named) while a one-org node falls back to the machine bearer. Amends ADR-029.
status: accepted
---

# ADR-035: A session reads mesh-context on its own bearer

2026-10-17, goal `one-node-serves-several-orgs-each-sandboxed-from-the-rest`,
item 5. Amends [ADR-029](adr-029-a-sessions-mcp-token-is-refreshed-by-the-node.md)
(a session's MCP token is refreshed by the node). Builds on
[ADR-034](adr-034-each-org-has-its-own-roots.md) (each org has its own
roots) and reuses runsheet ADR 056 (`adr-056-session-id-token-attester`,
the platform's half of Latchkey ADR 013's session tokens); it rebuilds
none of that chain. (Numbered 035 beside
[adr-035-a-node-low-on-disk-claims-nothing](adr-035-a-node-low-on-disk-claims-nothing.md),
as the goal's plan named it.)

## Context

ADR-029 gave every session a token file the node keeps fresh from
`gateway.Client.Bearer` — the node's machine bearer. On a node that
serves several orgs that bearer reads every one of them on `/mcp`: an
acme build's `mesh_search`, `mesh_items` or `mesh_document` could read
beta's documents and items. And the claim-time read, `Client.Session`,
dropped its `nodeID` — "the platform knows the node from the bearer" —
though one machine credential stands for a node id per served org.

runsheet ADR 056 already serves the narrower bearer: `POST
/mesh/v1/sessions` (on the node's own credential, by the node holding
the item's claim) opens the claim's `MeshSession` and answers a request
bearer (`rsr_…`) and a request URL; the URL mints, on that bearer alone,
an id token `/mcp` takes for that session — narrowed to its project,
refused the moment the claim ends.

## Decision

1. **The open.** `gateway.Client.OpenMeshSession(ctx,
   MeshSessionRequest{ItemID, ClaimSeq, NodeID})` posts `{item_id,
   claim_seq, node_id}` to `/mesh/v1/sessions` on the node's bearer,
   the node also named by the `X-Mesh-Node-Id` header
   (`gateway.NodeIDHeader`). The claim seq is the item's attempts when
   claimed, as the session's state names its claim (`SessionMeta.Attempt`)
   until `WorkItemIndex` serves `claimSeq`; the platform's own decides.
   A 404/405 is `IsUnsupported` (a platform older than the route); a
   4xx a refusal (`*gateway.Error`); `MeshSession.String` never names the
   request bearer. `MeshSessionToken(ctx, s, audience)` mints one id
   token at the request URL for `audience` — the relay's own `--url`,
   `MeshMCPURL` — and its life.
2. **The bearer is the session's.** `node.SessionBearers.Open(ctx, item,
   nodeID)` opens the build's session before the harness starts and
   mints its first id token (a lease the platform will not mint for
   starts nothing), then `TokenFiles.Bind(item, attempt, src)` binds a
   source that re-mints when less than `MeshSessionRefresh` (120 s,
   ADR 056 §6) of the token's life is left. The harnesses keep the file
   through `harness.SessionTokenKeeper.KeepSession(path, kind, meta)`:
   `TokenFiles` keeps it from the bearer bound for the job's own claim,
   and every refresh rewrites each file from its own source alone — a
   session's bearer never lands in another's file.
3. **Under its org's MCP root.** The Runner names the job's
   `TokenDir`, `MeshSessionTokenDir(<org roots>.MCP, item, attempt)` —
   `~/.mesh/orgs/<org>/mcp/sessions/<item>-<attempt>/runsheet.token` on
   a node serving several orgs (ADR-034's `OrgRoots.MCP`), and a host
   session's file goes there in place of `<workdir>.session/`. A pod's
   file stays in the pod's HOME, the one place the pod can read, still
   from its own session's bearer; a Codex session's goes in `TokenDir`.
4. **Cleared at the end.** When the build returns the binding is
   removed (the bearer forgotten; a file still kept for it stops
   refreshing rather than fall back to the machine's), the keeper's
   release removes the file, and — unless the session detached for an
   upgrade — its directory. An adopted build re-opens its claim's
   session (ADR 056: a second open replaces the bearer) and binds it
   before `Attach`, so `keepAdopted` keeps the file from it.
5. **Old platform.** Without the route a node serving one org keeps
   today's machine bearer, said once per run (`the platform opens no
   mesh sessions; …`). A node serving several orgs refuses: the build
   is released — never failed — with `mesh session: the platform opens
   no mesh sessions (POST /mesh/v1/sessions), and a node serving <n>
   orgs gives no session the machine bearer`
   (`MachineBearerRefusal`), and `TokenFiles.KeepSession` refuses a
   build with no bound bearer on such a node
   (`ErrMachineBearerRefused`) as a second guard. Any other failure to
   open is the release's reason the same way.
6. **The claim-time read names its node.** `Client.Session(ctx, ref,
   nodeID)` sends `nodeID` as `X-Mesh-Node-Id` on the read instead of
   discarding it, so a credential standing for several node ids is
   resolved to the one holding the claim.

## What this leaves

- Evaluate, rebase, plan and research sessions keep the machine
  bearer: ADR 056 gives a session only to a build claim ("an evaluation
  and a rebase … keep their node's credential until an amendment adds
  `lease: evaluate | rebase`"; a plan claim is not a work item). On a
  node serving several orgs they still read every served org's MCP
  data until that amendment lands, and channel mode's one session per
  org likewise.
- ADR 056 §6 has the relay hold the id token in memory
  (`--id-token-request`, the request bearer in the session's env). Here
  the id token rides ADR-029's token file, 0600 under the org's root,
  so the relay and the pod's RelayScript are unchanged; moving the mint
  into the relay is a later step.
- `mesh_finish`/`mesh_verdict` stay on the node's own `mesh-node`
  server and credential, as ADR 056's allow-list says.

## Consequences

- An acme build's `/mcp` reads are acme's project alone; beta's
  documents, items and health are refused it by the platform, whatever
  the node serves.
- A multi-org node on a platform without the route builds nothing and
  says why on every release; a one-org node is unchanged.
- Each build costs one open and one mint per ten minutes of its life,
  each recorded on its session by the platform (`MeshSessionMinted`).
