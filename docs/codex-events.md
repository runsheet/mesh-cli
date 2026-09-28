---
title: What `codex exec --json` reports
summary: codex-cli 0.157.0's JSONL stream, verified on an API-key login on 2026-09-25 — final message, thread id and token totals, no model, no dollars and no plan limits — and how harness.ParseCodexStream reads it.
---

# What `codex exec --json` reports

The Codex adapter (goal `codex-adapter`) runs OpenAI's Codex CLI as
`codex exec --json` and reads its stdout. Everything here comes from
real runs, saved verbatim under `internal/harness/testdata/codex_exec_*.jsonl`,
and from the CLI's own source at the same tag
(`codex-rs/exec/src/exec_events.rs`, `event_processor_with_jsonl_output.rs`).
`harness.ParseCodexStream` (`internal/harness/codexstream.go`) turns the
stream into a `harness.Result`.

## Verified against

- **codex-cli 0.157.0** (`rust-v0.157.0`, released 2026-09-25), the
  `codex-package-x86_64-unknown-linux-musl` release tarball. This node
  has no Node.js, so `npm i -g @openai/codex` was not an option. The
  bare `codex-x86_64-unknown-linux-musl` binary also works, but without
  `codex-code-mode-host` beside it every session starts with a warning
  item (`codex_exec_item_error.jsonl`).
- **Date:** 2026-09-25, linux/amd64 (Ubuntu, kernel 6.8).
- **Auth:** API key only. A temporary `CODEX_HOME` held just a copy of
  `~/.codex/auth.json` (`"auth_mode": "apikey"`, an `OPENAI_API_KEY`).
  CODEX_HOME was under `~/.cache`, because Codex refuses to create its
  helper binaries under `/tmp` and warns. **This machine has no
  ChatGPT-plan login**, so no plan run was recorded
  (see [Plan rate limits](#plan-rate-limits)).
- **Commands**, each run in a scratch git repo with stdin closed
  (`</dev/null`). Without that, `exec` waits on stdin for more prompt
  (`Reading additional input from stdin...` on stderr).

  | Fixture | Command |
  |---|---|
  | `codex_exec_apikey.jsonl` | `CODEX_HOME=$H codex exec --json --sandbox workspace-write -m gpt-6-astra 'Reply with exactly the words: hello from codex'` |
  | `codex_exec_item_error.jsonl` | the same, without `-m` (the default model, `gpt-6-astra`), and before `codex-code-mode-host` was installed |
  | `codex_exec_tools.jsonl` | `CODEX_HOME=$H codex exec --json --sandbox danger-full-access -m gpt-6-astra 'Run `ls` in this repo, then create a file NOTES.md containing the single line "noted", then reply with exactly: done'` |
  | `codex_exec_error.jsonl` | `CODEX_HOME=$H codex exec --json --sandbox workspace-write -m no-such-model 'Reply with exactly the words: hello from codex'` (exit 1) |

- **`--full-auto` no longer exists** in 0.157.0: `error: unexpected
  argument '--full-auto' found`. It used to mean
  `--sandbox workspace-write` with on-request approval. `exec` never asks
  for approval (the thread records `approval_mode: never`), so
  `--sandbox workspace-write` is the same thing now.
- **The workspace-write sandbox did not work on this host.** Codex's
  bundled bubblewrap fails with `bwrap: loopback: Failed RTM_NEWADDR:
  Operation not permitted`, because Ubuntu sets
  `kernel.apparmor_restrict_unprivileged_userns = 1`. Every write and
  shell command was blocked (the `file_change` item ends
  `"status":"failed"`), and the model reported that in its message.
  The run still ended in `turn.completed` with exit 0. The tools fixture
  was therefore recorded with `danger-full-access` in the throwaway
  repo. A node that builds on Codex needs an AppArmor profile for
  Codex's bwrap, or has to run it unsandboxed inside its own. The
  README's `## Codex on a node` says so.
- **An API key in the environment is `CODEX_API_KEY`.** In a
  `CODEX_HOME` with no `auth.json`, `exec` with only `OPENAI_API_KEY`
  set sends no credential and fails with a 401 (`Missing bearer or
  basic authentication`). With `CODEX_API_KEY` set it runs. The
  adapter (`harness.Codex`) sets both and writes no `auth.json` for an
  API-key account.
- **MCP servers from `config.toml`.** `[mcp_servers.<name>]` with `url`
  and `env_http_headers = { "Authorization" = "<VAR>" }` in the
  temporary `CODEX_HOME` reached a local test server with the header
  taken from `$VAR`. Under `--sandbox workspace-write` a *call* to one
  also needs `default_tools_approval_mode = "approve"` in its table; see
  [Why the context tool failed in Codex](#why-the-context-tool-failed-in-codex). `-c mcp_servers.<name>.…` overrides also work, but
  a dotted `-c` path takes no quoted segment.
- The only redaction: `/home/chris/` → `/home/user/` in
  `codex_exec_item_error.jsonl`. Ids, request ids and cf-rays are left
  as recorded.

## Events

One JSON object per line on stdout. Diagnostics go to stderr, never
stdout. Every line has `type`:

| `type` | Fields | Notes |
|---|---|---|
| `thread.started` | `thread_id` | First line. The session id (`codex exec resume <id>`), a UUIDv7. |
| `turn.started` | — | One per turn; `exec` runs one turn. |
| `item.started` | `item` | An item in progress (commands, file changes, tool calls). |
| `item.updated` | `item` | A change to an item in progress (todo lists). Seen in the source, not in the runs. |
| `item.completed` | `item` | A finished item. Agent messages and errors arrive only this way. |
| `turn.completed` | `usage` | The turn ended; see [Token usage](#token-usage). |
| `turn.failed` | `error.message` | The turn ended in an error; `exec` exits 1. |
| `error` | `message` | Top-level. A retry notice (`Reconnecting... 2/5 (…)`) or the final error just before `turn.failed`. Not fatal by itself. |

An `item` is `{id: "item_N", type, …}`, where `type` is one of:

| item `type` | Fields | Seen |
|---|---|---|
| `agent_message` | `text` | yes |
| `command_execution` | `command` (e.g. `/bin/bash -lc ls`), `aggregated_output`, `exit_code` (null while running), `status`: `in_progress`/`completed`/`failed`/`declined` | yes |
| `file_change` | `changes: [{path, kind: add/delete/update}]`, `status`: `in_progress`/`completed`/`failed` | yes |
| `error` | `message` | yes: a warning, and the turn goes on |
| `reasoning` | `text` | source only |
| `mcp_tool_call` | `server`, `tool`, `arguments`, `result` (`{content: [{type, text}], structured_content}` or null), `error` (`{message}` or null), `status` | source only (`codex_mcp_call_error.jsonl` is modelled on it) |
| `collab_tool_call` | `tool` (`spawn_agent`/`send_input`/`wait`/`close_agent`), `sender_thread_id`, `receiver_thread_ids`, `prompt`, `agents_states`, `status` | source only |
| `web_search` | `id`, `query`, `action`, `results` | source only |
| `todo_list` | `items: [{text, completed}]` | source only |

A completed `mcp_tool_call`'s own `id` (the item's `id`, above) is kept
on `harness.Result.ToolCallIDs[server]` in call order, alongside its
count on `ToolCalls[server]`, for `recordToolUsage`'s `toolCallId`; an
item with no id gets one synthesised (`<sessionId>:<server>:<n>`).

**Errors and wall time land on `Result.Tools[server]`** (a
`harness.ToolStat`), read from `Result.ToolReport()`: `item.started`
opens the clock for the call (keyed on its id, falling back to the
server when the item carries none) and `item.completed` closes it,
adding the elapsed time to `Wall`. The call is counted on `Calls`
whatever its `status` — `in_progress`/`completed`/`failed`/`declined`,
as `command_execution` uses them too — but a `status` of `failed`
also counts an `Errors` and, when it is the server's first, keeps its
text as `FirstError` (capped at 300 characters): the item's
`error.message` (a bare-string `error` is taken too), else the text
of its `result`'s content blocks, else the status word itself.
`ToolReport()` carries the first 200 characters as the row's `Error`,
which rides the usage report's `tools` entry as `error` and the item's
note (adr-006, amendment for goal
codex-sessions-reach-the-context-tool). `Codex.Run` redacts it of the
job's and the home's secrets before it leaves the harness. No
`item.started` before the matching `item.completed` (the fixtures
here never miss one, but a real stream might) leaves that call's own
share of `Wall` at zero.

**An MCP server that fails to start** makes no `mcp_tool_call` at
all: Codex says so in an `error` item (or a top-level `error`), e.g.
``MCP client for `prologue` failed to start: Auth required during MCP
startup`` when the server's `env_http_headers` variable is empty. A
message about MCP that names a server — ``MCP client for `<s>` ``,
`The <s> MCP server …`, ``MCP server `<s>` ``, or `MCP startup … (failed:
<a>, <b>)` — counts one error for each named server, once, with the
message as `FirstError` and zero calls
(`codex_mcp_startup_error.jsonl`), as a Claude server the init event
names not connected does. Only `Auth required during MCP startup` was
seen by hand on 0.157.0 (an empty header variable); the surrounding
wording and both fixtures (`codex_mcp_call_error.jsonl`,
`codex_mcp_startup_error.jsonl`) are modelled on codex-cli's source,
not captured from a node session — the first captured one should
replace them. A message about MCP that names no server is not counted.

**No event names the model.** The model shows up only in Codex's own
state (`$CODEX_HOME/state_5.sqlite`, the session file under
`$CODEX_HOME/sessions/`). So `ParseCodexStream` takes the model the
session was started on, which is the `-m` the adapter passes.
According to the source, when the server moves a session to another
model, the stream carries only an `error` item,
`model rerouted: <from> -> <to> (<reason>)`. No reroute happened in
these runs, so the parser does not act on it yet: such a session is
priced at the model it asked for.

`ParseCodexStream` hands every parsed line to `onEvent` as a
`CodexEvent` and skips lines that are not JSON or not objects. It
ignores types it does not know.

## Final message

It is the `text` of the last `item.completed` whose item is an
`agent_message`. A turn can have several (`codex_exec_tools.jsonl`:
`I’ll list the repo contents and create `NOTES.md`.` and then `done`).
This matches the CLI's own `final_message` and `--output-last-message`.
`Result.Text` is that text, trimmed. A failed turn usually has none,
and then `Text` is empty; the reason is on `turn.failed`'s
`error.message`, and the caller gets it through `onEvent`.

`Result.Success` is true when a `turn.completed` arrived and no
`turn.failed` did. Error items and top-level `error` retries do not
change it. `Result.EmptyOutput` is set when the stream held no
parseable event, exactly as `ParseStream` defines it. `Result.Turns`
counts `turn.completed`.

## Token usage

`turn.completed.usage`:

```json
{"input_tokens":25133,"cached_input_tokens":12481,"cache_write_input_tokens":12646,"output_tokens":78,"reasoning_output_tokens":0}
```

- `input_tokens` **includes** both `cached_input_tokens` (read from the
  prompt cache) and `cache_write_input_tokens` (written to it). The
  tools run shows it: 12481 + 12646 = 25127 of 25133, so 6 tokens were
  plain input. On a first run almost the whole prompt is a cache write
  (12621 of 12624).
- `output_tokens` includes `reasoning_output_tokens`, as in the
  Responses API.
- The usage is **the thread's running total**, not the turn's own
  share: the CLI emits `usage.total` from its last token-usage update.
  The parser keeps the last `turn.completed` rather than adding them up.
  Only `exec resume` has more than one.

These land on the new `Result.Usage` (`harness.Usage`): the model, the
five counts, and `Unpriced`.

## Cost

Codex reports tokens, not dollars. `CostUSD` is computed from OpenAI's
**standard API list prices** in `harness.CodexPrices`, keyed by model,
in USD per million tokens. Source: <https://developers.openai.com/api/docs/pricing>,
read 2026-09-25.

| Model | Input | Cached input | Cache write | Output |
|---|---|---|---|---|
| `gpt-6-astra` (Codex's default) | 10.00 | 1.00 | 12.50 | 50.00 |
| `gpt-6-sol` | 2.00 | 0.20 | 2.50 | 10.00 |
| `gpt-6-luna` | 0.10 | 0.01 | 0.125 | 0.50 |
| `gpt-5.3-codex` | 1.75 | 0.175 | — (billed as input) | 14.00 |

```
cost = ((input − cached − cache_write) × Input + cached × CachedInput
        + cache_write × CacheWrite + output × Output) / 1e6
```

The API-key fixture comes to 3 × 10 + 12621 × 12.50 + 8 × 50 =
**$0.1581925**, and the tools fixture to **$0.174516**. The GPT-6 models
charge long-context rates (2× input, 1.5× output) on a request over 272K
input tokens. That cannot happen under Codex: it caps the context
window at 258,400 (`model_context_window` in the session file), so the
table holds only the short-context rates. A model missing from the
table costs 0 and sets `Usage.Unpriced`. The node should log that
rather than count the session as free. The table has to be updated by
hand when OpenAI's prices change.

## Plan rate limits

**`codex exec --json` carries no rate-limit information, on any
login.** The exec event schema has no field for it (`exec_events.rs`
at 0.157.0), so `ParseCodexStream` leaves `Result.RateLimit` nil. The
session file Codex writes under `$CODEX_HOME/sessions/` (nested by
date, `rollout-*.jsonl`) has it instead: an `event_msg` / `token_count`
line's `rate_limits` field. On an API-key login it is `null`
(`codex_session_ratelimits_null.jsonl`). On a ChatGPT-plan login it is a
`RateLimitSnapshot`: `primary` and `secondary` windows of
`{used_percent 0–100, window_minutes, resets_at (epoch s)}`, plus
`plan_type`, `credits` and `limit_id`.

**`harness.ParseCodexSessionLimits(home string) (*harness.RateLimit,
error)`** (`internal/harness/codexlimits.go`) reads it: the newest
`*.jsonl` under `home/sessions`, its last line carrying a non-null
`rate_limits`, `primary`/`secondary` placed onto `RateLimit.FiveHour`/
`SevenDay` by each window's own `window_minutes` (300 → `FiveHour`,
10080 → `SevenDay`; `plan_type` is not mapped onto `RateLimit` today),
`used_percent` become each window's `Utilization` (0–1), and `Status`
is `"allowed"` — a snapshot exists only while sessions may run. No
sessions directory, no `.jsonl` file, or none carrying a snapshot is
`(nil, nil)`, not an error. `Codex.Run` calls it after the exec ends
and sets `Result.RateLimit` from it when the stream carried none (which
it always does today), so a Codex session paces itself as a Claude one
does (`internal/node/limits.go`).

**The shape is from codex-cli 0.157.0's source
(`codex-rs/protocol/src/protocol.rs`'s `RateLimitSnapshot`), unverified
on a live ChatGPT-plan login as of 2026-09-28**: this machine still has
none, so `internal/harness/testdata/codex_session_ratelimits.jsonl` (a
plan snapshot) and `codex_session_ratelimits_null.jsonl` (the null
case, matching the API-key run above) are modelled on the source and
the doc above, not captured from a run. The first live plan run should
replace them and update this note.

**Only API dollars are counted for Codex** regardless: a plan session's
list-price cost is a measure of use, not money spent
(`harness.CodexPrices`); the rate-limit reading above is what paces a
plan account, not what it costs.

## Why the context tool failed in Codex

On 2026-09-28 (mesh-cli 9ed5f8b, codex-cli 0.157.0) every call to the
built-in context server inside a Codex evaluation on the mini failed
(`prologue: 3 calls, 3 errors` in 31–92 ms, `1 call, 1 error` in 2 ms),
though the same `config.toml` with the node's `rs_` token worked from
`codex exec` by hand. Each candidate the goal named, and what decided it
(goal `codex-sessions-reach-the-context-tool`, item 4):

| Candidate | Verdict | Evidence |
|---|---|---|
| Codex's approval policy for MCP calls in `exec` | **ruled in: the cause** | A run on the mini with the harness's own argv (`codex exec --json --sandbox workspace-write -m gpt-6-luna -`), a fresh `CODEX_HOME` holding a copy of a ChatGPT login and the harness's `config.toml` (`[mcp_servers.prologue]`, `url = "https://api.runsheet.dev/mcp"`, `env_http_headers = { "Authorization" = "MESH_CODEX_MCP_0_TOKEN" }`), and `MESH_CODEX_MCP_0_TOKEN=Bearer <the node's rs_ token>`: the model found `mesh_documents` (the server started and listed its tools) and the call ended `failed` with `MCP tool call requires approval, but approval policy is never` — refused inside the process, which is why it took milliseconds. Kept verbatim as `internal/harness/testdata/codex_mcp_approval_error.jsonl` (`TestParseCodexStreamKeepsTheApprovalRefusal`). The same run with `default_tools_approval_mode = "approve"` in the server's table: two calls, both `completed`, and the answer `146 documents`. |
| `--sandbox workspace-write` | **ruled in, as the condition** | The same config and token under `--sandbox danger-full-access`, no approval key: both calls `completed`. `exec` approves MCP calls itself only when there is no sandbox to protect; under `workspace-write` a call needs approval, and exec's policy is `never`. A by-hand run that works is one run outside the sandbox. The node keeps `workspace-write` and approves instead. |
| Console tools rendered as `${MESH_TOOL_<NAME>_<KEY>}` | **ruled in as a defect, not this failure** | `SessionTools.Apply` renders console tools for Claude Code, and `prepareHome` fed that map to `CodexMCPConfig`, so a console tool's secret header reached Codex as a literal `Bearer ${…}` — Codex expands no `${VAR}`. It did not cause the prologue errors: the built-in's header is a literal value, and the evaluations that failed served `playwright` and `context7`, which carry no secret. Fixed: the harness renders `job.Tools` with `tools.CodexConfig` and `CodexMCPConfig` resolves a `${VAR}` from the job's env or refuses it (`TestCodexEvaluationReceivesResolvedTools`, `TestCodexMCPConfigResolvesOrRefusesRefs`). |
| The Unset list (`codexInherited`, `CodexAuthEnv`) against `job.Env` | ruled out | `SessionEnviron` drops only *inherited* variables and appends `job.Env` after, so nothing the account or the harness sets is unset. `TestCodexEvaluationReceivesResolvedTools` runs the issued-subscription path with the operator's `CODEX_HOME` and `OPENAI_API_KEY` set: the fake `codex` sees the account's home, no key, and the built-in's variable holding `Bearer <the node token>`. |
| `prepareHome`'s job-home path (an issued account) vs the harness-owned home | ruled out | Both paths write the same `config.toml` and the same header variables (`codexConfig`); the test above takes the job-home path an issued account takes. |
| The token on the built-in (`gw.Bearer`) | ruled out | On the mini `~/.mesh/token` is the operator's `rs_` console token, which does not expire; the recorded run used it and `/mcp` accepted it once the call was approved. A missing or refused token looks different: `Auth required during MCP startup` (an empty variable) or `HTTP status client error (401 Unauthorized)`, never an approval refusal. |
| A startup timeout on the mini | ruled out | The server started and listed its tools in every recorded run; the failure is on the call (`item.started` then a `failed` `item.completed`), not a startup `error` item. `startup_timeout_sec` is not set. |
| `codex_apps` erroring too | not reproduced | Codex's own ChatGPT apps server, not one the node renders, so `default_tools_approval_mode` on the node's tables does not reach it. Its 2.6 s includes a network start; presumably the same approval refusal. The item's note now names its first error, so the next live run shows it. |

**What the live capture should show** once the fixed node runs a Codex
evaluation: the kept stream's `mcp_tool_call` items for `prologue` end
`"status":"completed"` with a result, the usage record's `tools` entry
reads `prologue: N calls, 0 errors`, and the evaluation's notes carry no
`tools: prologue: first call errored`. If a call still fails, its
`error.message` is on the tool entry and the note — an approval refusal
means the key did not take (check `config.toml` in the kept session
home); a 401 means the token.

Codex writes to the home's `config.toml` itself: after a run it has
appended `[projects."<workspace>"] trust_level = "trusted"`. That is
harmless, since the node writes the file afresh for every session.

## Errors

- **Unknown or inaccessible model** (`codex_exec_error.jsonl`, exit 1):
  first an `error` item, `Model metadata for `no-such-model` not found.
  Defaulting to fallback metadata…`. Then top-level `error` retries
  `Reconnecting... N/5 (unexpected status 404 Not Found: The model
  `no-such-model` does not exist or you do not have access to it.)`,
  over WebSockets, then an `error` item `Falling back from WebSockets
  to HTTPS transport…`, then five more retries over HTTPS carrying the
  `url`, `cf-ray` and `request id`. Last come a final `error` and a
  `turn.failed` with the same message. There is no `turn.completed` and
  no usage: `Success=false`, `CostUSD=0`, `Usage=nil`, and `SessionID`
  is still the thread id.
- **Warnings as error items** (`codex_exec_item_error.jsonl`): a missing
  `codex-code-mode-host` gives `Code Mode is unavailable…` as an
  `item.completed` error before `turn.started`. The turn then succeeds.
  Error items never fail a session.
- **Blocked sandbox**: a failed `file_change` or command is an item
  status, not an error. The session "succeeds", and the model's final
  message says what it could not do. Deciding whether the work was
  done is the evaluator's job.
- **Exit status**: 0 after `turn.completed`, 1 after `turn.failed`; the
  CLI exits 2 on a bad argument, before any JSON. An adapter should
  treat a non-zero exit with no `turn.failed` as a failure too.
