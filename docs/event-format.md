---
title: Mesh's event format — stream-json, and what Codex's app-server stands in for it
summary: The stream-json event types and fields harness.Event (internal/harness/stream.go) parses, and how internal/nodev1/codexsession.go's codexTranslator rebuilds the same lines from codex app-server's own notifications — line by line, naming what Codex has no equivalent for.
---

# Mesh's event format

`claude -p --output-format stream-json --verbose` writes one JSON
object per line on stdout. `harness.Event` (`internal/harness/stream.go`)
is that line, and `harness.ParseStream` reads a whole stream of them
into a `harness.Result`. This is the one format a lease's transcript
upload and liveness and account tracking (`stoprules.go`, `lease.go`)
read, whichever harness produced it (see "Codex app-server", below):
"a product parses one format" (specs/mesh-v1-gaps-for-products.md §3).

## The event types `harness.Event` parses

Every line has `type`; `ParseStream` acts on five of them, and leaves
any other `type` it does not recognise alone (`onEvent` still sees it,
but nothing on `Result` changes).

| `type` | `subtype` | Fields `Event` parses | What `ParseStream` does with it |
|---|---|---|---|
| `system` | `init` (the only one `ParseStream` acts on) | `session_id`, `model`, `mcp_servers: [{name, status}]` | Keeps the session id; `model` becomes the model later usage is priced and reported on; each `mcp_servers` entry whose `status` is not `"connected"` counts an error on that server's `Result.Tools` entry (`FirstError` the status word) even with zero calls, and a `"connected"` one is remembered so a later `is_error` `tool_result` can tell a server's own answer from Claude Code refusing the call before the server saw it. |
| `assistant` | — | `message.id`, `message.role`, `message.usage` (`input_tokens`, `cache_creation_input_tokens`, `cache_read_input_tokens`), `message.content[]`: `{type: "text", text}`, `{type: "tool_use", id, name, input}` | `Text()` joins the `text` blocks for `Result.Text`'s fallback. `message.usage` counts one turn's input tokens, once per `message.id` (the CLI can send more than one event for the same message, a content block each). Each `tool_use` of an MCP server's tool (`mcp__<server>__<tool>`) is counted on `Result.ToolCalls`, `Result.ToolCallIDs`, `Result.ToolNames` and opens that call's wall-clock on `Result.Tools`; a mesh-work tool's (`tools.IsWorkTool`) own input is kept on `Result.WorkCalls`. |
| `user` | — | `message.content[]`: `{type: "tool_result", tool_use_id, is_error, content}` | Matches `tool_use_id` back to the `tool_use` that opened it: closes that call's wall-clock on `Result.Tools`, counts `Clean` or `Errors` (and, on an error, keeps the first 300 characters of the result text as `FirstError`, and counts `Answered` when the server was connected and the text is the server's own answer rather than Claude Code's refusal of the call — `claudeToolAnswered`), and keeps the result's byte length among `Result.LargestToolResults` by the matching `tool_use`'s own name. |
| `rate_limit_event` | — | `rate_limit_info: {status, resetsAt, rateLimitType, overageStatus, isUsingOverage, unifiedWindows: {five_hour, seven_day}}` | The latest one becomes `Result.RateLimit` (`Event.RateLimit`, `internal/harness/ratelimit.go`). |
| `result` | `success` / anything else | `is_error`, `result`, `total_cost_usd`, `num_turns`, `usage: {input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens}`, `modelUsage` | Ends the stream's reading: `Result.Success` is `!is_error && subtype == "success"`; `CostUSD`, `Turns` and `Text` (trimmed `result`) are taken from it; `usage` becomes `Result.Usage` (`InputTokens` here includes the cached ones). A stream with no `result` event leaves `Success` false and `Text` the last assistant text seen. |

A line that is not valid JSON, or does not start with `{` (Claude
Code's own diagnostics on stdout), is skipped. A stream with no
parseable JSON line at all sets `Result.EmptyOutput`.

## Codex app-server's own notifications

`internal/nodev1/codexsession.go`'s `CodexSession` runs `codex
app-server` instead of `claude -p` (`internal/harness/codexsession`,
the JSON-RPC transport) and rewrites its notifications into the same
stream-json line shape, through `codexTranslator.observe`, so
everything above — `harness.Event`, `ParseStream`, and the code that
reads either one's output — needs no Codex-specific branch. This is a
different Codex integration from `codex exec --json`
(`docs/codex-events.md`, read by `harness.ParseCodexStream` for the
`harness.Codex` evaluation adapter): `app-server`'s notifications are
JSON-RPC (`{"method": ..., "params": ...}`, no top-level `type`), not
stream-json lines, so `codexTranslator` builds the stream-json line by
hand rather than passing one through.

| app-server notification | `observe`'s stream-json line | Codex has no equivalent for |
|---|---|---|
| `agentMessage` (`params.text`, the turn's running text) | an `assistant` event whose `message.content` is `[{type: "text", text}]` — the turn's text so far, not just this delta | — |
| `item.started` where `item.type == "command_execution"` (`params.item.id`, `.command`) | an `assistant` event with one `tool_use` block, `id` the item's id, `name` `"command_execution"`, `input.command` the item's `command` | every other `item.type` (`file_change`, `reasoning`, `mcp_tool_call`, `web_search`, `todo_list`, …, see docs/codex-events.md) — `observe` answers no line for it |
| `item.completed` where `item.type == "command_execution"` (`.id`, `.status`, `.aggregated_output`) | a `user` event with one `tool_result` block, `tool_use_id` the item's id, `is_error` from `status == "failed"`, `content` the item's `aggregated_output` | same as above: any other item type's `item.completed` answers no line |
| `turn.completed` (`params.usage`, a `harness.CodexUsage`) | a `result` event, `subtype: "success"`, `result` the turn's text, `num_turns` the session's running turn count, `total_cost_usd` the running cost (`harness.CodexPrices`), `usage` the running `input_tokens`/`cache_read_input_tokens`/`cache_creation_input_tokens`/`output_tokens` | a model missing from `harness.CodexPrices` prices at 0 and sets `Usage.Unpriced`, same as the exec adapter |
| `turn.failed` (`params.error.message`, `params.usage`) | the same `result` shape with `subtype: "error"`, `is_error: true`, `result` the turn's last text, and the failure kept on `codexTranslator.failReason` for `CodexSession.Run`'s own error, not on the line | — |
| any other method (`thread.started`'s own notification shape — app-server answers the thread id on `thread/start`'s RPC response instead — `item.updated`, `item.started`/`item.completed` for a non-`command_execution` item, and anything app-server adds later) | no line (`observe`'s `default` case) | app-server emits no `system`/`init`-equivalent notification at all: `codexTranslator` is given the model on `CodexSession.Run`'s own `spec.Model` instead of reading it off the stream, and no notification carries `mcp_servers` connection status, a `rate_limit_event`, or a `modelUsage` breakdown by model — `CodexSession`'s `SessionResult` carries no `RateLimit` and `codexTranslator`'s own `usage` is the one model `spec.Model` named, never a multi-model `modelUsage` |

`CodexSession` also has no model ladder to escalate on
(`MsgEscalate` is only logged, `codexsession.go`'s own doc comment) —
app-server has no `set_model` request — which is why no stream-json
line stands in for a model change either.

Both `agentMessage`'s running text and the `tool_use`/`tool_result`
pair close over the matching ids the same way a Claude Code stream
does, so `stoprules.Watch`'s tool-call and tool-error counts need no
Codex-specific code: a `command_execution` item is read as if it were
a `Bash` tool call.
