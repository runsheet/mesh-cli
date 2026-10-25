---
title: "ADR-038: A session reaches every MCP server through one proxy"
summary: Each build, evaluation, plan and research session reaches the built-in runsheet and every console tool through its own sessionproxy (internal/sessionproxy) — streamable HTTP at /mcp/<server> on a 0600 unix socket alone in a directory under the session's org MCP root, run by the node on the host and holding every credential (the session's runsheet bearer in memory, each runsheet request sent `Authorization: DPoP` with a fresh proof by the node's in-memory key and a proof refusal answered -32603, never retried, a remote tool's headers, a local tool's secret env in the child it runs) — so the agent's MCP config names only a secretless shim (a pod's node script, a host's `mesh mcp-relay --socket --server`), the pod mounts the socket's directory alone, and no runsheet.token, MESH_TOOL_* or secret value is in the agent's env, disk or config; a server's failure is named by the proxy's own error, state.json names the socket and server names so an adopting node restarts the proxy on the same path, and TestSessionProxySandboxedE2E proves it in a real pod. Amends ADR-029 and ADR-035; for a node's sessions it supersedes mesh-specs ADR 006 decision 2, secrets in env only.
status: accepted
---

# ADR-038: A session reaches every MCP server through one proxy

2026-10-19, goal `a-session-reaches-every-mcp-server-through-one-proxy`
(items 1–6). Amends
[ADR-029](adr-029-a-sessions-mcp-token-is-refreshed-by-the-node.md) (a
session's MCP token is refreshed by the node) and
[ADR-035](adr-035-a-session-reads-mesh-context-on-its-own-bearer.md) (a
session reads mesh-context on its own bearer). Builds on
[ADR-006](adr-006-session-tools-rendered-by-the-node.md) (the node
renders a session's tools), [ADR-007](adr-007-the-sandbox-on-the-node.md)
(the sandbox), [ADR-009](adr-009-sessions-outlive-the-node.md) (sessions
outlive the node) and [ADR-034](adr-034-each-org-has-its-own-roots.md)
(each org has its own roots).

**For a node's sessions this supersedes mesh-specs ADR 006 decision 2,
"secrets in env only".** The amendment to mesh-specs ADR 006 itself is
the `runsheet/mesh-specs` repository's work, not this one's; until it
lands, this ADR is what a node does.

## Context

Every credential a session's MCP servers used reached the agent. A
console tool's secret header or env rode `MESH_TOOL_<NAME>_<KEY>` in the
agent's environment (ADR-006, mesh-specs ADR 006 decision 2), referenced
from its MCP config; the built-in runsheet's bearer was a file,
`runsheet.token`, in the pod's HOME (or `<workdir>.session/`, or the
CODEX_HOME), that the in-pod relay read per request (ADR-029), holding
the session's own mesh session bearer (ADR-035). An agent that ran
`env`, `cat ~/runsheet.token` or `grep -r mesh-session /` read them, and
anything it printed or committed could carry them. Secrets in env kept
them off the command line and out of config files — not out of the
agent's reach.

The relay also failed opaquely. dl360-anythink's node.log carried
`tools: runsheet served, never called: failed` on item sessions: Claude
Code's init event says only that the stdio server `failed`, and the
node could say nothing better because the failure was inside the pod.

## Decision

1. **One proxy per session, on the host** (`internal/sessionproxy`,
   items 1–2). `sessionproxy.Proxy` serves streamable-HTTP MCP at
   `/mcp/<server>` on a unix socket (`Listen`, mode 0600, a stale socket
   removed first), one upstream per server name:
   - `Remote{URL, Headers}` — a console tool's URL, its headers filled
     from what the proxy holds; a client's `Authorization`, and any
     header the upstream config names, is dropped, never forwarded;
   - `Runsheet{URL, Bearer, Key}` — the built-in, its bearer asked of
     `Bearer` per request and held only in memory. With `Key` (the
     node's in-memory `dpop.Key` the session's mesh-session token was
     minted bound to, `TokenFiles.KeyOf` → `SessionProxies.keyOf` →
     `upstreamsOf`) every request goes as `Authorization: DPoP <token>`
     with a fresh proof of its method and URL and the token's hash —
     its own `jti`, never reused — and a `DPoP` header the agent sent
     is dropped (runsheet ADR 064 §2, §5); without one, `Bearer`. A 401
     `invalid_token` asks again and retries once, with a fresh proof; a
     401 whose `WWW-Authenticate` is a DPoP challenge with
     `error="invalid_dpop_proof"` (`dpopRefusal`) is the proof refused,
     not a stale token: never retried, the agent answered JSON-RPC
     -32603 `sessionproxy: <name>: upstream refused the DPoP proof:
     <reason>`, masked (amended 2026-10-25, goal
     `the-relay-signs-each-mcp-request-with-its-sessions-key`);
   - `Local{Command, Args, Env, Secrets}` — a local tool (`npx -y
     <package>`, `uvx`, `docker run -i`, as `internal/tools` renders the
     runtime's launch) started by the proxy on the host, lazily on the
     first request, restarted once (initialize replayed), its secrets in
     that child's env alone; its stdio bridged by ids of the proxy's
     own, each answer returned to its caller under the caller's id,
     what it says unasked out on a GET event stream, its process group
     killed on `Close`.

   Only the MCP headers are carried; `Mcp-Session-Id` both ways; an
   event stream relayed as it arrives. Every reply, error, log line and
   a local child's stderr is masked of the secrets. The proxy records
   each server's first error (`FirstError`).

2. **The agent's config names only a shim** (item 3).
   `tools.ProxiedServers` renders every server — remote, local and the
   built-in — as one shape of entry from a caller's builder, and returns
   apart from it an `UpstreamSpec` per server carrying what the proxy
   reaches it on. A host session's entry is `harness.ProxyRelay`, `mesh
   mcp-relay --socket <socket> --server <name>` (`internal/mcprelay`'s
   socket mode: no token file, no Authorization header); in a pod, which
   has no mesh binary, the harness rewrites each to
   `harness.ProxiedServer`, `node <HOME>/mcp-proxy-shim.js <socket>
   <name>` (`ProxyShimScript`, Node's `http.request` with `socketPath`).
   Neither's argv, file or env names a credential.

3. **The node starts it** (item 4). `SessionTools.UseVia` with the
   node's `SessionProxies` replaces `UseTools` for build, evaluation,
   plan and research sessions: it opens a proxy on
   `<org MCP root>/px/<rand>/s` (`ProxySocketName`, short for a socket
   path's 108 bytes; the org's MCP root under ADR-034, else the node's,
   so `CheckedMounts` accepts it), the runsheet upstream's bearer from
   `TokenFiles.Source` — the session's bound mesh session bearer for a
   build (ADR-035), else the machine bearer, refused as before for a
   build of a multi-org node — and sets every server on the job as a
   `ProxyRelay`, `Job.Tools` nil and the values on `Job.Secrets` for
   redaction. A pod mounts the socket's directory alone
   (`Session.ShareProxyDir`), and a proxied session's pod admits no
   `MESH_TOOL_*` variable; no `runsheet.token` is written for it. The
   proxy ends with the session (`SessionProxy.End`).

4. **A failure is named by the proxy.** `SessionAccount.UseNote` asks the
   proxy's `FirstError` first, so a runsheet upstream that fails reads
   `tools: runsheet served, never called: sessionproxy: runsheet: POST
   <url> answered 503`, never the agent's bare `failed`.

5. **An adopted session's proxy is restarted** (item 5). `state.json`
   names the socket and the server names (`sessionunit.Proxy`,
   `harness.ProxyStateOf`), never an upstream's URL, header, secret or
   bearer. A node shutting down with a session detached stops the proxy
   but keeps its directory (`SessionProxy.Stop`) — a pod's mount; the
   adopting node — build, evaluation, plan or research — re-reads the
   session's tools at claim, re-binds its bearer, and starts a proxy on
   the same socket (`SessionProxies.Adopt`, `adoptProxy`), logged
   `<label>: proxy restarted on <socket> (<servers>)`, a server no
   longer served named `no longer served: …`. A node with no proxy logs
   that the adopted session's MCP servers go unanswered.
   `keepAdoptedToken` keeps no token file for a proxied session.

6. **Proved in a real pod** (item 6).
   `internal/node/session_proxy_sandboxed_e2e_test.go`'s
   `TestSessionProxySandboxedE2E` (`-tags e2e`, `MESH_SANDBOX_E2E=1`,
   step 2b of `scripts/e2e-sandbox.sh`, [docs/e2e-sandbox.md](../e2e-sandbox.md))
   runs `Runner.RunItem` on podman with the node image: a stub agent
   calls runsheet, a remote tool and a local tool through the pod shim;
   fake upstreams on the host record the credential each got; `podman
   exec` greps the running agent container's whole filesystem for
   `mesh-session` and for each secret and finds nothing; the build's
   notes carry no `runsheet served, never called`; no pod or proxy
   directory is left.

## Consequences

- The agent cannot read a credential its MCP servers use: not in its
  env, not on its disk, not in its config. It can still *use* them —
  every call goes through the proxy — which is what it was given them
  for.
- ADR-029's token file and its 15-second rewrite are no longer a
  proxied session's: the proxy asks its bearer per request, so rotation
  needs nothing on disk. ADR-029 still holds for rebase and channel
  sessions, which keep runsheet's relay alone, and for a session an
  older node started and this one adopted (its `tokenFile` kept).
- ADR-035's session bearer is unchanged in what it is; only where it
  lives moves, from a file under the session's org MCP root to the
  proxy's memory.
- ADR-006's rendering (`ClaudeCodeServers`, `CodexConfig`, secrets as
  `${MESH_TOOL_…}`) stays for a node with no `SessionProxies` and for
  rebase and channel sessions; tests and older nodes use it.
- A Codex session's preflight lists the proxied built-in through its
  shim and checks no token file.
- A local tool runs on the host, outside the pod: the host needs its
  runtime (`npx`, `uvx`, `docker`), as `Loop.Runtimes` already reports,
  and the tool's own file access is the node user's, not the pod's.
  A local tool that should be confined is a later decision.
- What is left: `mesh node start` gives `SessionProxies` a single-org
  MCP root until it sets `Roots`; channel-mode sessions, not in a pod,
  are not proxied; the mesh-specs amendment to ADR 006 decision 2 is
  owed by `runsheet/mesh-specs`.

## Amendment 2026-10-25: the relay signs each request with its session's key

Goal `the-relay-signs-each-mcp-request-with-its-sessions-key` (runsheet
ADR 064, a mesh session token is DPoP-bound). `SessionBearers` mints a
build's token for audience `mesh-session` (`MeshSessionAudience`) on a
DPoP proof of the node's `dpop.Key`, held in memory for the process's
life, and binds the key beside the bearer (`TokenFiles.Bind`,
`KeyOf`); the session's proxy gets it as `Runsheet.Key`, so every /mcp
request carries `Authorization: DPoP` and a fresh proof, and a proof
refusal is -32603 naming its reason, not retried (Decision 1 above).
The tests that pin it are `TestRelaySignsEachRequest`: in
`internal/sessionproxy`, `fresh_proof_per_request` (each request a
proof `dpoptest.Verifier` accepts, distinct `jti`s, the agent's DPoP
dropped), `invalid_token_retries/once` (a 401 `invalid_token` is
forwarded again exactly once — the retry's 200 reaches the client, two
401s give two upstream requests and no third),
`invalid_token_retries/fresh_proof` and `proof_refusal`; in
`internal/node`, end to end — the mint through gatewaytest asks
`mesh-session` with a proof bound to the node's key, `upstreamsOf`
gives the runsheet upstream that key, two calls through the session's
socket reach a verifying /mcp each with a fresh proof (ath of the
minted token, jkt the key's thumbprint), and a refusal reads -32603.
