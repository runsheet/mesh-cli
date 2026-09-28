---
summary: The node renders each session's tools (internal/tools) in the agent's own format — Claude Code mcpServers beside the built-in mesh-context server, allowed by --allowedTools mcp__<name>__*, and Codex [mcp_servers.<name>] tables in a 0600 config.toml under the session's CODEX_HOME (console tools by tools.CodexConfig, the built-in by CodexMCPConfig with any ${VAR} resolved or refused, every server's tools approved for exec's workspace-write sandbox) — with every secret value in the session's environment only (MESH_TOOL_<NAME>_<KEY>, or the key itself for a Codex local tool's env_vars), the configs carrying variable names, a clash with the built-in refused, and a local tool whose runtime the node lacks omitted when nice-to-have (noted on the item) or declining the build (released, not reclaimed while the node's runtimes are unchanged) when required; priced tool calls, counted per MCP server from the stream, go to the platform as recordUsage records (calls × pricePerCallCents) after a build or evaluation.
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
   platform's `itemTools(itemId)` (`planTools(goalId)`,
   `researchTools(directionId)`) beside the credentials field of the
   same subject (ADR-005). Amended 2026-09-27: the platform serves each
   tool as runsheet ADR 020 fixes it — `{id, slug, name, kind
   remote_mcp|local_mcp|builtin, need required|nice_to_have, url,
   command, args, runtime, builtin, builtIn, account {id, label} |
   null, headers, env}` with headers and env as `[{name, value} |
   {name, source: "item_github_token"}]` — and the node maps it to its
   `Tool`: the slug is the name, `remote_mcp` is remote and `local_mcp`
   local, `need` gives `required`; the platform's own built-in
   (`builtIn`, mesh-context, which the node attaches itself as
   prologue) and a builtin switch are not tools of the read, nor is a
   row whose slug no renderer takes. Every setting of a tool served
   through an account is a secret (it may be that account's; a plain
   header travelling as a variable costs nothing), a tool served with
   none has plain headers and env, and a setting sourced from the
   item's GitHub token reads `${GH_TOKEN}`, which the session's
   environment carries (ghcred.go) and both agents expand. A
   `pricePerCallCents` is taken when the platform serves one.
   `SessionCredentials` is the read's credentials alone. A gateway that
   refuses the tools field is asked for the credentials alone from then
   on (per field), and its sessions get no console tools. `Loop.LookPath`
   (`exec.LookPath` in `mesh node start`) probes runtimes and adapters
   every tick, beside the reload, and the heartbeat carries them as
   `runtimes` and `adapters` (`HeartbeatExtras`), each only when
   non-empty. The probe is a PATH lookup; it does not run the version
   command trd §1 names.
8. **At claim.** `Credentials.Open` makes the claim-time read and keeps
   its tools on the `SessionAccount`. The build (`Runner.RunItem`) and
   the evaluation (`Evaluator.Evaluate`) select them for their role and
   the node's runtimes (`SelectTools`, which also turns a tool either
   renderer refuses — a clash with `prologue`, a bad or repeated name —
   into an omission noted on the item) and render the rest for Claude
   Code beside the built-in Prologue attachment (`SessionTools.Apply`):
   `Job.MCPServers`, the values on `Job.Env` and `Job.Secrets`, and the
   tools themselves on `Job.Tools` for an adapter that writes its own
   config (Codex, `tools.CodexConfig` into its `CODEX_HOME`; see the
   item 4 amendment below). Plan,
   research, rebase and channel sessions keep the built-in alone.
9. **Omit and decline.** A nice-to-have omission is a progress note on
   the item (`tools: …`), or a line of an evaluation's verdict notes. A
   missing required tool releases a build (`ReleaseWorkItem`, the
   reason naming each tool and its runtime) and records a `Declines`
   entry keyed on the item, the missing tools and the runtimes; the loop
   neither claims nor evaluates that item again while the node's
   runtimes are unchanged, for `DeclineHold` (an hour) at most, since
   the item's tools may change in the console meanwhile. An item in
   `evaluating` cannot be released (the platform releases only a claimed
   or running item), so an evaluation declined for its tools records no
   verdict and waits the same way. `Loop.Runtimes` probes once before
   the first tick, so an item adopted at start sees the node's runtimes.
10. **Redaction.** `Redact(s, env)` masks every value of the returned
   env in `s` as `[redacted]`, the longest first; whatever the node logs
   or reports about a tool session passes through it.

11. **Priced calls.** The harness counts a session's MCP calls per
   server on `harness.Result.ToolCalls` (Claude Code's `tool_use`
   named `mcp__<server>__…`, Codex's completed `mcp_tool_call` by its
   `server`). The build and the evaluation hand their rendered tools to
   the `SessionAccount` (`SessionTools.UseTools`), and its `Finish`
   calls `RecordToolUsage` — mutation `recordToolUsage`, input
   `aggregateId` (the item), `toolCallId` (from
   `Result.ToolCallIDs`), `tool` and `usage` (the session's report,
   ADR-005, at the tool's `pricePerCallCents`, no tokens) — once per
   call of a tool priced above zero. (Amended 2026-09-27: it called
   `recordUsage` once per tool, a field only the platform may call.)
   Prologue and unpriced tools are never reported. A gateway without
   the mutation (`IsUnsupported`) is said once per run and each call
   logged as a `usage: tool …` line; any error is logged, never the
   item's failure.

*Amended 2026-09-28 (goal `evaluations-run-on-codex`, item 2):* a
Codex session's `CODEX_HOME` may be its account's rather than the
harness's own: when the claim-time read issues an openai credential
serving codex, `PrepareCodexAccount` (ADR-005) makes the home, puts the
login's `auth.json` in it and names it on `Job.Env`, and the Codex
harness's `prepareHome` writes the tools' `config.toml` into that same
home — secrets still by variable name in it, their values on the env —
and leaves the directory to the account's `Cleanup`. The tools are
rendered no differently.

*Amended 2026-09-28 (goal `builtin-is-mesh-context`, item 5):* the
built-in is `mesh-context` (`tools.Builtin`), not `prologue` — the
product's own rename (prologue-becomes-mesh) reached the node's own
attachment. `ClaudeCode` renders it under the key `mesh-context`,
`harness.MCPArgs` allows `mcp__mesh-context__*`, and a console tool
named `mesh-context` is now the clash `check` refuses; one named
`prologue` no longer clashes and is rendered like any other console
tool. `gateway.servedTool` still leaves the platform's `builtIn: true`
row out of the read regardless of its slug, so the built-in is never
rendered twice either way. For one release, a session adopted from an
older node (ADR-009) may still carry an MCP config naming `prologue`
and call `mcp__prologue__*`; the node's priced-usage accounting
(`SessionAccount.reportToolUsage`) treats both names as the built-in
(unpriced) until this note goes, 2026-10-28.

## Consequences

- No secret value is on argv (`--mcp-config` carries names), in the
  `--mcp-config` JSON or `config.toml`, or in a log; tests assert it.
- A crash leaves configs that are safe to read; the environment dies
  with the session.
- Codex local secrets are visible to the Codex process itself under
  their own names, not only to the tool; an org sharing one key name
  across two tools with different values cannot run both under Codex.
- A declined evaluation holds its item on this node until the runtime
  appears or the hold lapses; handing an evaluation to another node
  needs a platform mutation that does not exist yet.
- A priced call's cost is its list price as the console states it; the
  node does not know what the tool's provider actually charged.

## Amendment — 2026-09-28 (goal tools-show-their-use)

Nobody could tell from the console whether a served tool was working:
only priced calls were recorded, and none is priced. Every MCP call is
now counted per tool, and a tool that is served but unused or failing
says so on the item.

- **`tools` on the usage report.** The harness counts each MCP server's
  calls, errors (a `tool_result` with `is_error`, a failed Codex
  `mcp_tool_call`, a server the init event names as not connected) and
  wall time (`Result.ToolReport()`). `gateway.UsageReport` carries them
  as `tools: [{tool, calls, errors, ms}]` beside the total `toolCalls`,
  on every finishing command's `usage`; `SessionAccount.Add` merges the
  rows by tool across the sessions one command carries (the Codex second
  opinion), summing calls, errors and ms. Prologue is a row like any
  other server. `recordToolUsage`'s per-call record sends no list.
- **The fallback.** A platform whose `UsageReportInput` has no `tools`
  refuses the field: `mutateWithUsage` sends the command again with the
  list taken out and the total kept, remembers that for the client's
  later reports, and returns `ErrNoToolUsage`. `SessionAccount.Carried`
  takes that as a landed command; `Finish` says once per run `usage:
  the platform takes no per-tool calls yet; they are logged here
  instead` and logs each session's list.
- **The two notes.** `SessionTools.UseNote` names, from the console
  tools rendered for the session (never the built-in Prologue), each one
  served but never called — `tools: brave served, never called` — and
  each whose calls errored, by its first error — `tools: perplexity:
  first call errored: <text>` — the text redacted by the account and of
  the tool's own secret and header values, capped at 200 characters;
  empty when every served tool was called cleanly. A build sends it on
  the item through `ReportWorkItemProgress` after its final note, an
  evaluation adds it to its verdict's notes beside the omitted tools,
  and a plan or research session, with no item to note it on, logs it.

## Amendment — 2026-09-28 (goal codex-sessions-reach-the-context-tool)

Codex evaluations showed `prologue: 3 calls, 3 errors` and the note
`prologue: first call errored: failed` — the error counted, its reason
lost, and the built-in, which the note deliberately left out, the one
tool that mattered. Both change.

- **The error's text.** A failed Codex `mcp_tool_call` keeps its
  `error.message` (a bare string accepted), else its `result`'s text,
  else its status, as the server's `FirstError` (300 characters); a
  Codex `error` item or top-level `error` naming an MCP server that
  failed to start (``MCP client for `prologue` failed to start: …``)
  counts an error for that server with the message, even with no call,
  as Claude's `not connected` init status already did
  (`docs/codex-events.md`). `Codex.Run` redacts the text of the job's
  and the home's secrets.
- **On the tool entry.** `Result.ToolReport()` rows and
  `gateway.ToolCallReport` carry `error`: the first 200 characters of
  `FirstError`, redacted again by the account and of every served
  tool's values in `SessionAccount.Add`; merging a carried session's
  rows keeps the first non-empty one. A platform whose tools entry does
  not know `error` would refuse the whole list, so the client sends it
  only when introspection of `UsageReportInput` finds `error` on the
  `tools` entry (asked only when a report carries one, kept an hour, a
  failed probe a no) and strips it otherwise: the report never falls
  to the `ErrNoToolUsage` resend for it.
- **The built-in in the note.** `SessionTools.UseNote` still names the
  served console tools as before, then every other server the result
  names with an error — the built-in `prologue` first, then Codex's own
  (`codex_apps`) by name — `prologue: first call errored: <text>`, or
  `prologue served, never called: <text>` for one that failed to
  start, redacted and capped as above. A built-in called cleanly is
  still never named. The evaluation's notes and a build's progress
  note so say why the session could not read the project.

## Amendment — 2026-09-28 (goal codex-sessions-reach-the-context-tool, item 4)

Codex sessions got the tools rendered for Claude Code. `SessionTools.Apply`
puts every console tool on `Job.MCPServers` with its secrets as
`${MESH_TOOL_<NAME>_<KEY>}`. The Codex harness fed that map to
`CodexMCPConfig`, and `WriteCodexConfig` (decision 4), the renderer
meant for Codex, was never called. Codex expands no `${VAR}`, so a
console tool's secret header went out as the literal text. Separately,
every call to the built-in was refused before it left the process:
`codex exec` runs with approval policy `never`, and under `--sandbox
workspace-write` an MCP call needs approval (`docs/codex-events.md`,
"Why the context tool failed in Codex", records the run on the mini).
So the Codex half of decision 4 now works like this:

- **The Codex renderer renders console tools.** The harness's
  `codexConfig(job)` writes the session's `config.toml` in two parts.
  First come the job's own servers, the built-in Prologue: every server
  on `Job.MCPServers` that no `Job.Tools` entry names, by
  `CodexMCPConfig`, each header value in a `MESH_CODEX_MCP_<n>_TOKEN`
  variable. Then come the console tools, `Job.Tools`, by
  `tools.CodexConfig` (what `WriteCodexConfig` writes, now also
  returned as text): a bearer `Authorization` goes by
  `bearer_token_env_var`, other secret headers by `env_http_headers`,
  a local tool's secrets by `env_vars`. The values are in the
  session's environment only. Their Claude Code entries on
  `Job.MCPServers` are not used for Codex. Both the harness-owned home
  and an issued account's home (the 2026-09-28 amendment above) take
  this path.
- **No `${` reaches Codex.** `CodexMCPConfig(servers, jobEnv)` resolves
  every `${VAR}` in a url, header, command, arg or env value from the
  job's environment. A reference the environment does not set is an
  error, and the session does not start, rather than a literal header.
- **The node's servers are approved.** Every `[mcp_servers.<name>]`
  either renderer writes carries `default_tools_approval_mode =
  "approve"` (`tools.CodexApproval`). The node chose the servers, and
  a session that cannot call them judges blind. The sandbox stays
  `workspace-write`. Codex's own `codex_apps` is not rendered by the
  node and is not approved by it.
