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
  taken from `$VAR`. `-c mcp_servers.<name>.…` overrides also work, but
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
| `mcp_tool_call` | `server`, `tool`, `arguments`, `result`, `error`, `status` | source only |
| `collab_tool_call` | `tool` (`spawn_agent`/`send_input`/`wait`/`close_agent`), `sender_thread_id`, `receiver_thread_ids`, `prompt`, `agents_states`, `status` | source only |
| `web_search` | `id`, `query`, `action`, `results` | source only |
| `todo_list` | `items: [{text, completed}]` | source only |

A completed `mcp_tool_call`'s own `id` (the item's `id`, above) is kept
on `harness.Result.ToolCallIDs[server]` in call order, alongside its
count on `ToolCalls[server]`, for `recordToolUsage`'s `toolCallId`; an
item with no id gets one synthesised (`<sessionId>:<server>:<n>`).

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
at 0.157.0). The session file Codex writes under `$CODEX_HOME/sessions/`
has an `event_msg` / `token_count` with a `rate_limits` field. On this
API-key login it was `null`. On a ChatGPT-plan login the source says
it is a `RateLimitSnapshot`: `primary` and `secondary` windows of
`{used_percent 0–100, window_minutes, resets_at (epoch s)}`, plus
`plan_type`, `credits` and `limit_id`. That would map onto
`harness.RateLimit` with `primary` (300 minutes) → `FiveHour` and
`secondary` (10080 minutes) → `SevenDay`, choosing each window by its
`window_minutes`. **None of that is verified**: no plan login exists on
this machine and it is not in the stream the adapter reads.

So `ParseCodexStream` leaves `Result.RateLimit` nil, and **only API
dollars are counted for Codex.** A plan session's list-price cost is a
measure of use, not money spent. Feeding plan windows to the headroom
logic needs a plan fixture first, read from the session file or from
`codex app-server`'s rate-limit notifications. It is a follow-up, not
part of this parser.

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
