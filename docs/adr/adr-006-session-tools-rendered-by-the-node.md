---
summary: The node renders each session's tools (internal/tools) in the agent's own format — Claude Code mcpServers beside the built-in prologue server, allowed by --allowedTools mcp__<name>__*, and Codex [mcp_servers.<name>] tables in a 0600 config.toml under the session's CODEX_HOME — with every secret value in the session's environment only (MESH_TOOL_<NAME>_<KEY>, or the key itself for a Codex local tool's env_vars), the configs carrying variable names, a clash with the built-in refused, and a local tool whose runtime the node lacks omitted when nice-to-have or reported as missing when required.
status: accepted
---

# ADR-006: A session's tools are rendered by the node, secrets in env only

## Context

mesh-specs ADR 006 and trd §3 Tools decide that the platform hands a
claimed session its tools (`name`, `transport` remote|local, `url` +
`headers` | `runtime` + `command`/`args`/`env`, `required`, `roles`,
`pricePerCallCents`) with their secrets decrypted for the claiming node
only, and that the node writes them in each agent's own format with
secrets in the environment only. This records how mesh-cli does that:
where the code lives, the variable names, and the rendering per agent.

## Decision

1. **One leaf package.** `internal/tools` holds the `Tool` (JSON tags as
   the trd's table, plus `secrets`: a header or env key and its value,
   which wins over the same key in `headers`/`env`), the probes
   (`ProbeRuntimes` for npx/uvx/docker, `ProbeAdapters` for
   claude-code/codex — found by their CLIs `claude` and `codex` — both
   over an injected `lookPath`), `Select`, the two
   renderers and `Redact`. It imports nothing from `node` or `gateway`,
   so the loop, the heartbeat and each harness can use it alike.
2. **Select by role, then by runtime.** A tool whose `roles` leave the
   session's role out is dropped. A local tool whose runtime the node
   lacks is an `Omission` with a human note — in `omitted` when
   nice-to-have (the session runs without it; the note goes on the
   item), in `missingRequired` when required (the node declines the
   claim). Remote tools need no runtime.
3. **Claude Code.** `ClaudeCode(builtin, tools)` returns the
   `mcpServers` map — the built-in servers (Prologue) and one entry per
   tool: remote `{type: http, url, headers}`, local `{command, args,
   env}` with `npx -y <command> …`, `uvx <command> …` or
   `docker run -i --rm -e <KEY>… <image> …` — so `harness.MCPArgs` of it
   allows `mcp__<name>__*` for each. A secret is written `${VAR}` (a
   bearer `Authorization` as `Bearer ${VAR}`), which Claude Code expands
   from its environment; the values come back as `env`.
4. **Codex.** `WriteCodexConfig(codexHome, tools)` writes
   `codexHome/config.toml`, mode 0600, one `[mcp_servers.<name>]` per
   tool: remote `url`, plain `http_headers`, a bearer `Authorization`
   as `bearer_token_env_var` and any other secret header in
   `env_http_headers`; local `command`, `args`, plain `env` and secret
   keys in `env_vars`. Codex forwards `env_vars` under their own names,
   so a local tool's secret travels under its key, and two tools giving
   one key different values is an error.
5. **Variable names.** Every other secret travels as
   `MESH_TOOL_<NAME>_<KEY>` (upper-cased, anything not `[A-Z0-9]` as
   `_`); a bearer token is carried without its `Bearer ` prefix so both
   agents read the same value.
6. **Refusals.** A tool name not `[a-z0-9_-]`, named twice, or equal to
   a built-in (`prologue` always) is an error from either renderer, as
   is an unknown transport or runtime. Nothing is written then.
7. **The wiring.** The claim-time read is `gateway.Session` — the
   `sessionCredentials` mutation's `credentials` and `tools` (headers,
   env and secrets as maps); `SessionCredentials` is its credentials
   alone. A gateway that refuses the `tools` selection is asked for the
   credentials alone from then on, and its sessions get no console
   tools. `Loop.LookPath` (`exec.LookPath` in `mesh node start`) probes
   runtimes and adapters every tick, beside the reload, and the
   heartbeat carries them as `runtimes` and `adapters`
   (`HeartbeatExtras`), each only when non-empty. The probe is a PATH
   lookup; it does not run the version command trd §1 names.
8. **Redaction.** `Redact(s, env)` masks every value of the returned
   env in `s` as `[redacted]`, the longest first; whatever the node logs
   or reports about a tool session passes through it.

## Consequences

- No secret value is on argv (`--mcp-config` carries names), in the
  `--mcp-config` JSON or `config.toml`, or in a log; tests assert it.
- A crash leaves configs that are safe to read; the environment dies
  with the session.
- Codex local secrets are visible to the Codex process itself under
  their own names, not only to the tool; an org sharing one key name
  across two tools with different values cannot run both under Codex.
- The rest of the wiring (rendering at claim, the decline, priced-call
  usage) builds on this package and decision 7 in the goal's later
  items.
